package cluster

import (
	"engine"
	"errors"
	"fmt"
	"parser"
	"protocol"
	"sort"
	"strings"
	"time"
	"wal"
)

// A shard imements an interface for writing and querying data.
// It can be copied to multiple servers or the local datastore.
// Shard contains data from [startTime, endTime)
// Ids are unique across the cluster
type Shard interface {
	Id() uint32
	StartTime() time.Time
	EndTime() time.Time
	Write(*protocol.Request) error
	Query(querySpec *parser.QuerySpec, response chan *protocol.Response) error
	IsMicrosecondInRange(t int64) bool
}

// Passed to a shard (local datastore or whatever) that gets yielded points from series.
type QueryProcessor interface {
	// This method returns true if the query should continue. If the query should be stopped,
	// like maybe the limit was hit, it should return false
	YieldPoint(seriesName *string, columnNames []string, point *protocol.Point) bool
	Close()
}

type NewShardData struct {
	Id            uint32 `json:",omitempty"`
	StartTime     time.Time
	EndTime       time.Time
	ServerIds     []uint32
	Type          ShardType
	DurationSplit bool `json:",omitempty"`
}

type ShardType int

const (
	LONG_TERM ShardType = iota
	SHORT_TERM
)

type ShardData struct {
	id              uint32
	startTime       time.Time
	startMicro      int64
	endMicro        int64
	endTime         time.Time
	wal             WAL
	servers         []wal.Server
	clusterServers  []*ClusterServer
	store           LocalShardStore
	serverIds       []uint32
	shardType       ShardType
	durationIsSplit bool
	shardDuration   time.Duration
	localServerId   uint32
	IsLocal         bool
}

func NewShard(id uint32, startTime, endTime time.Time, shardType ShardType, durationIsSplit bool, wal WAL) *ShardData {
	return &ShardData{
		id:              id,
		startTime:       startTime,
		endTime:         endTime,
		wal:             wal,
		startMicro:      startTime.Unix() * int64(1000*1000),
		endMicro:        endTime.Unix() * int64(1000*1000),
		serverIds:       make([]uint32, 0),
		shardType:       shardType,
		durationIsSplit: durationIsSplit,
		shardDuration:   endTime.Sub(startTime),
	}
}

const (
	PER_SERVER_BUFFER_SIZE  = 10
	LOCAL_WRITE_BUFFER_SIZE = 10
)

var (
	queryResponse       = protocol.Response_QUERY
	endStreamResponse   = protocol.Response_END_STREAM
	queryRequest        = protocol.Request_QUERY
	dropDatabaseRequest = protocol.Request_DROP_DATABASE
)

type LocalShardDb interface {
	Write(database string, series *protocol.Series) error
	Query(*parser.QuerySpec, QueryProcessor) error
	DropDatabase(database string) error
}

type LocalShardStore interface {
	Write(request *protocol.Request) error
	SetWriteBuffer(writeBuffer *WriteBuffer)
	BufferWrite(request *protocol.Request)
	GetOrCreateShard(id uint32) (LocalShardDb, error)
	ReturnShard(id uint32)
	DeleteShard(shardId uint32) error
}

func (self *ShardData) Id() uint32 {
	return self.id
}

func (self *ShardData) StartTime() time.Time {
	return self.startTime
}

func (self *ShardData) EndTime() time.Time {
	return self.endTime
}

func (self *ShardData) IsMicrosecondInRange(t int64) bool {
	return t >= self.startMicro && t < self.endMicro
}

func (self *ShardData) SetServers(servers []*ClusterServer) {
	self.clusterServers = servers
	self.servers = make([]wal.Server, len(servers), len(servers))
	for i, server := range servers {
		self.serverIds = append(self.serverIds, server.Id)
		self.servers[i] = server
	}
	self.sortServerIds()
}

func (self *ShardData) SetLocalStore(store LocalShardStore, localServerId uint32) error {
	self.serverIds = append(self.serverIds, localServerId)
	self.localServerId = localServerId
	self.sortServerIds()

	self.store = store
	// make sure we can open up the shard
	_, err := self.store.GetOrCreateShard(self.id)
	if err != nil {
		return err
	}
	self.store.ReturnShard(self.id)
	self.IsLocal = true

	return nil
}

func (self *ShardData) ServerIds() []uint32 {
	return self.serverIds
}

func (self *ShardData) Write(request *protocol.Request) error {
	request.ShardId = &self.id
	requestNumber, err := self.wal.AssignSequenceNumbersAndLog(request, self)
	if err != nil {
		return err
	}
	request.RequestNumber = &requestNumber
	if self.store != nil {
		self.store.BufferWrite(request)
	}
	for _, server := range self.clusterServers {
		server.BufferWrite(request)
	}
	return nil
}

func (self *ShardData) WriteLocalOnly(request *protocol.Request) error {
	requestNumber, err := self.wal.AssignSequenceNumbersAndLog(request, self)
	if err != nil {
		return err
	}
	request.RequestNumber = &requestNumber
	self.store.BufferWrite(request)
	return nil
}

func (self *ShardData) Query(querySpec *parser.QuerySpec, response chan *protocol.Response) error {
	// This is only for queries that are deletes or drops. They need to be sent everywhere as opposed to just the local or one of the remote shards.
	// But this boolean should only be set to true on the server that receives the initial query.
	if querySpec.RunAgainstAllServersInShard {
		if querySpec.IsDeleteFromSeriesQuery() {
			return self.logAndHandleDeleteQuery(querySpec, response)
		} else if querySpec.IsDropSeriesQuery() {
			return self.logAndHandleDropSeriesQuery(querySpec, response)
		}
	}

	if self.IsLocal {
		var processor QueryProcessor
		if querySpec.IsListSeriesQuery() {
			processor = engine.NewListSeriesEngine(response)
		} else if querySpec.IsDeleteFromSeriesQuery() || querySpec.IsDropSeriesQuery() || querySpec.IsSinglePointQuery() {
			maxDeleteResults := 10000
			processor = engine.NewPassthroughEngine(response, maxDeleteResults)
		} else {
			if self.ShouldAggregateLocally(querySpec) {
				processor = engine.NewQueryEngine(querySpec.SelectQuery(), response)
			} else {
				maxPointsToBufferBeforeSending := 1000
				processor = engine.NewPassthroughEngine(response, maxPointsToBufferBeforeSending)
			}
		}
		shard, err := self.store.GetOrCreateShard(self.id)
		if err != nil {
			return err
		}
		defer self.store.ReturnShard(self.id)
		shard.Query(querySpec, processor)
		processor.Close()
		return err
	}

	healthyServers := make([]*ClusterServer, 0, len(self.clusterServers))
	for _, s := range self.clusterServers {
		if !s.IsUp() {
			continue
		}
		healthyServers = append(healthyServers, s)
	}
	healthyCount := len(healthyServers)
	if healthyCount == 0 {
		message := fmt.Sprintf("No servers up to query shard %d", self.id)
		response <- &protocol.Response{Type: &endStreamResponse, ErrorMessage: &message}
		return errors.New(message)
	}
	randServerIndex := int(time.Now().UnixNano() % int64(healthyCount))
	server := healthyServers[randServerIndex]
	request := self.createRequest(querySpec)

	return server.MakeRequest(request, response)
}

func (self *ShardData) DropDatabase(database string, sendToServers bool) {
	if self.IsLocal {
		if shard, err := self.store.GetOrCreateShard(self.id); err == nil {
			defer self.store.ReturnShard(self.id)
			shard.DropDatabase(database)
		}
	}

	if !sendToServers {
		return
	}

	responses := make([]chan *protocol.Response, len(self.clusterServers), len(self.clusterServers))
	for i, server := range self.clusterServers {
		responseChan := make(chan *protocol.Response, 1)
		responses[i] = responseChan
		request := &protocol.Request{Type: &dropDatabaseRequest, Database: &database, ShardId: &self.id}
		go server.MakeRequest(request, responseChan)
	}
	for _, responseChan := range responses {
		// TODO: handle error responses
		<-responseChan
	}
}

func (self *ShardData) String() string {
	serversString := make([]string, 0)
	for _, s := range self.servers {
		serversString = append(serversString, fmt.Sprintf("%d", s.GetId()))
	}
	local := "false"
	if self.IsLocal {
		local = "true"
	}

	return fmt.Sprintf("[ID: %d, LOCAL: %s, SERVERS: [%s]]", self.id, local, strings.Join(serversString, ","))
}

func (self *ShardData) ShouldAggregateLocally(querySpec *parser.QuerySpec) bool {
	if self.durationIsSplit && querySpec.ReadsFromMultipleSeries() {
		return false
	}
	groupByInterval := querySpec.GetGroupByInterval()
	if groupByInterval == nil {
		if querySpec.HasAggregates() {
			return false
		}
		return true
	}
	if self.shardDuration%*groupByInterval == 0 {
		return true
	}
	return false
}

func (self *ShardData) logAndHandleDeleteQuery(querySpec *parser.QuerySpec, response chan *protocol.Response) error {
	queryString := querySpec.GetQueryStringWithTimeCondition()
	request := self.createRequest(querySpec)
	request.Query = &queryString
	return self.LogAndHandleDestructiveQuery(querySpec, request, response, false)
}

func (self *ShardData) logAndHandleDropSeriesQuery(querySpec *parser.QuerySpec, response chan *protocol.Response) error {
	return self.LogAndHandleDestructiveQuery(querySpec, self.createRequest(querySpec), response, false)
}

func (self *ShardData) LogAndHandleDestructiveQuery(querySpec *parser.QuerySpec, request *protocol.Request, response chan *protocol.Response, runLocalOnly bool) error {
	requestNumber, err := self.wal.AssignSequenceNumbersAndLog(request, self)
	if err != nil {
		return err
	}
	var localResponses chan *protocol.Response
	if self.IsLocal {
		localResponses = make(chan *protocol.Response, 1)

		// this doesn't really apply at this point since destructive queries don't output anything, but it may later
		maxPointsFromDestructiveQuery := 1000
		processor := engine.NewPassthroughEngine(localResponses, maxPointsFromDestructiveQuery)
		shard, err := self.store.GetOrCreateShard(self.id)
		if err != nil {
			return err
		}
		defer self.store.ReturnShard(self.id)
		err = shard.Query(querySpec, processor)
		processor.Close()
		if err != nil {
			return err
		}
	}
	if !runLocalOnly {
		responses := make([]chan *protocol.Response, len(self.clusterServers), len(self.clusterServers))
		for i, server := range self.clusterServers {
			responseChan := make(chan *protocol.Response, 1)
			responses[i] = responseChan
			// do this so that a new id will get assigned
			request.Id = nil
			server.MakeRequest(request, responseChan)
		}
		for i, responseChan := range responses {
			for {
				res := <-responseChan
				if *res.Type == endStreamResponse {
					self.wal.Commit(requestNumber, self.clusterServers[i].Id)
					break
				}
				response <- res
			}
		}
	}

	if localResponses != nil {
		for {
			res := <-localResponses
			if *res.Type == endStreamResponse {
				self.wal.Commit(requestNumber, self.localServerId)
				break
			}
			response <- res
		}
	}

	response <- &protocol.Response{Type: &endStreamResponse}
	return nil
}

func (self *ShardData) createRequest(querySpec *parser.QuerySpec) *protocol.Request {
	queryString := querySpec.GetQueryString()
	user := querySpec.User()
	userName := user.GetName()
	database := querySpec.Database()
	isDbUser := !user.IsClusterAdmin()

	return &protocol.Request{
		Type:     &queryRequest,
		ShardId:  &self.id,
		Query:    &queryString,
		UserName: &userName,
		Database: &database,
		IsDbUser: &isDbUser,
	}
}

// used to serialize shards when sending around in raft or when snapshotting in the log
func (self *ShardData) ToNewShardData() *NewShardData {
	return &NewShardData{
		Id:        self.id,
		StartTime: self.startTime,
		EndTime:   self.endTime,
		Type:      self.shardType,
		ServerIds: self.serverIds,
	}
}

// server ids should always be returned in sorted order
func (self *ShardData) sortServerIds() {
	serverIdInts := make([]int, len(self.serverIds), len(self.serverIds))
	for i, id := range self.serverIds {
		serverIdInts[i] = int(id)
	}
	sort.Ints(serverIdInts)
	for i, id := range serverIdInts {
		self.serverIds[i] = uint32(id)
	}
}

func SortShardsByTimeAscending(shards []*ShardData) {
	sort.Sort(ByShardTimeAsc{shards})
}

func SortShardsByTimeDescending(shards []*ShardData) {
	sort.Sort(ByShardTimeDesc{shards})
}

type ShardCollection []*ShardData

func (s ShardCollection) Len() int      { return len(s) }
func (s ShardCollection) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

type ByShardTimeDesc struct{ ShardCollection }
type ByShardTimeAsc struct{ ShardCollection }

func (s ByShardTimeAsc) Less(i, j int) bool {
	if s.ShardCollection[i] != nil && s.ShardCollection[j] != nil {
		iStartTime := s.ShardCollection[i].StartTime().Unix()
		jStartTime := s.ShardCollection[j].StartTime().Unix()
		if iStartTime == jStartTime {
			return s.ShardCollection[i].Id() < s.ShardCollection[j].Id()
		}
		return iStartTime < jStartTime
	}
	return false
}
func (s ByShardTimeDesc) Less(i, j int) bool {
	if s.ShardCollection[i] != nil && s.ShardCollection[j] != nil {
		iStartTime := s.ShardCollection[i].StartTime().Unix()
		jStartTime := s.ShardCollection[j].StartTime().Unix()
		if iStartTime == jStartTime {
			return s.ShardCollection[i].Id() < s.ShardCollection[j].Id()
		}
		return iStartTime > jStartTime
	}
	return false
}
