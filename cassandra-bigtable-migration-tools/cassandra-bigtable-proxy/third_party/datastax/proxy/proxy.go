// Copyright (c) DataStax, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proxy

import (
	"bytes"
	"context"
	"crypto"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigtable"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	bigtableModule "github.com/GoogleCloudPlatform/cloud-bigtable-ecosystem/cassandra-bigtable-migration-tools/cassandra-bigtable-proxy/bigtable"
	otelgo "github.com/GoogleCloudPlatform/cloud-bigtable-ecosystem/cassandra-bigtable-migration-tools/cassandra-bigtable-proxy/otel"
	"github.com/GoogleCloudPlatform/cloud-bigtable-ecosystem/cassandra-bigtable-migration-tools/cassandra-bigtable-proxy/responsehandler"
	schemaMapping "github.com/GoogleCloudPlatform/cloud-bigtable-ecosystem/cassandra-bigtable-migration-tools/cassandra-bigtable-proxy/schema-mapping"
	"github.com/GoogleCloudPlatform/cloud-bigtable-ecosystem/cassandra-bigtable-migration-tools/cassandra-bigtable-proxy/third_party/datastax/parser"
	"github.com/GoogleCloudPlatform/cloud-bigtable-ecosystem/cassandra-bigtable-migration-tools/cassandra-bigtable-proxy/third_party/datastax/proxycore"
	"github.com/GoogleCloudPlatform/cloud-bigtable-ecosystem/cassandra-bigtable-migration-tools/cassandra-bigtable-proxy/translator"
	"github.com/GoogleCloudPlatform/cloud-bigtable-ecosystem/cassandra-bigtable-migration-tools/cassandra-bigtable-proxy/utilities"
	"github.com/datastax/go-cassandra-native-protocol/datatype"
	"github.com/datastax/go-cassandra-native-protocol/frame"
	"github.com/datastax/go-cassandra-native-protocol/message"
	"github.com/datastax/go-cassandra-native-protocol/primitive"
	lru "github.com/hashicorp/golang-lru"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	"golang.org/x/oauth2/google"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"
)

var (
	encodedOneValue, _ = proxycore.EncodeType(datatype.Int, primitive.ProtocolVersion4, 1)
	commitTsFn         = "PENDING_COMMIT_TIMESTAMP()"
)

var ErrProxyClosed = errors.New("proxy closed")
var ErrProxyAlreadyConnected = errors.New("proxy already connected")
var ErrProxyNotConnected = errors.New("proxy not connected")

const systemQueryMetadataNotFoundError = "data not found %s[%v]"

const selectType = "select"
const alterType = "alter"
const updateType = "update"
const insertType = "insert"
const deleteType = "delete"
const createType = "create"
const dropType = "drop"

const ts_column = "last_commit_ts"
const preparedIdSize = 16
const limitValue = "limitValue"
const Query = "Query"

const translatorErrorMessage = "Error occurred at translator"
const metadataFetchError = "Error while fetching table Metadata - "
const errorAtBigtable = "Error occurred at bigtable - "
const errorWhileDecoding = "Error while decoding bytes - "
const unhandledScenario = "Unhandled execution Scenario for prepared Query"
const errQueryNotPrepared = "query is not prepared"
const (
	handleQuery            = "handleQuery"
	handleBatch            = "Batch"
	handleExecuteForInsert = "handleExecuteForInsert"
	handleExecuteForDelete = "handleExecuteForDelete"
	handleExecuteForUpdate = "handleExecuteForUpdate"
	handleExecuteForSelect = "handleExecuteForSelect"
	cassandraQuery         = "Cassandra Query"
	bigtableQuery          = "Bigtable Query"
	rowKey                 = "Row Key"
)

var (
	systemSchema        = "system_schema"
	keyspaces           = "keyspaces"
	tables              = "tables"
	columns             = "columns"
	systemVirtualSchema = "system_virtual_schema"
	local               = "local"
)

// Events
const (
	executingBigtableRequestEvent       = "Executing Bigtable Mutation Request"
	executingBigtableSQLAPIRequestEvent = "Executing Bigtable SQL API Request"
	bigtableExecutionDoneEvent          = "bigtable Execution Done"
	gotBulkApplyResp                    = "Got the response for bulk apply"
	sendingBulkApplyMutation            = "Sending Mutation For Bulk Apply"
	// todo remove once we support ordered code ints
	encodeIntValuesWithBigEndian = true
)

type PeerConfig struct {
	RPCAddr string   `yaml:"rpc-address"`
	DC      string   `yaml:"data-center,omitempty"`
	Tokens  []string `yaml:"tokens,omitempty"`
}

type Config struct {
	Version        primitive.ProtocolVersion
	MaxVersion     primitive.ProtocolVersion
	RetryPolicy    RetryPolicy
	NumConns       int
	Logger         *zap.Logger
	RPCAddr        string
	DC             string
	Tokens         []string
	BigtableConfig bigtableModule.BigtableConfig
	OtelConfig     *OtelConfig
	ReleaseVersion string
	Partitioner    string
	CQLVersion     string
	// PreparedCache a cache that stores prepared queries. If not set it uses the default implementation with a max
	// capacity of ~100MB.
	PreparedCache proxycore.PreparedCache
	UserAgent     string
	ClientPid     int32
	ClientUid     uint32
}

type Proxy struct {
	ctx                      context.Context
	config                   Config
	logger                   *zap.Logger
	cluster                  *proxycore.Cluster
	sessions                 [primitive.ProtocolVersionDse2 + 1]sync.Map // Cache sessions per protocol version
	mu                       sync.Mutex
	isConnected              bool
	isClosing                bool
	clients                  map[*client]struct{}
	listeners                map[*net.Listener]struct{}
	eventClients             sync.Map
	preparedCache            proxycore.PreparedCache
	preparedIdempotence      sync.Map
	systemLocalValues        map[string]message.Column
	closed                   chan struct{}
	localNode                *node
	nodes                    []*node
	bClient                  bigtableModule.BigTableClientIface
	translator               *translator.Translator
	schemaMapping            *schemaMapping.SchemaMappingConfig
	otelInst                 *otelgo.OpenTelemetry
	otelShutdown             func(context.Context) error
	systemQueryMetadataCache *SystemQueryMetadataCache
}

type node struct {
	addr   *net.IPAddr
	dc     string
	tokens []string
}

func (p *Proxy) OnEvent(event proxycore.Event) {
	switch evt := event.(type) {
	case *proxycore.SchemaChangeEvent:
		p.logger.Debug("Schema change event detected", zap.String("SchemaChangeEvent", evt.Message.String()))
	}
}

func createBigtableConnection(ctx context.Context, config Config) (map[string]*bigtable.Client, map[string]*bigtable.AdminClient, error) {

	// Initialize Bigtable client
	connConfig := bigtableModule.ConnConfig{
		InstanceIDs:   config.BigtableConfig.InstanceID,
		NumOfChannels: config.BigtableConfig.NumOfChannels,
		GCPProjectID:  config.BigtableConfig.GCPProjectID,
		AppProfileID:  config.BigtableConfig.AppProfileID,
		UserAgent:     config.UserAgent,
	}

	bigtableClients, adminClients, err := bigtableModule.CreateClientsForInstances(ctx, connConfig)
	if err != nil {
		config.Logger.Error("Failed to create Bigtable client: " + err.Error())
		return nil, nil, err
	}
	return bigtableClients, adminClients, nil
}

func createBigtableSqlConnection(ctx context.Context, config Config) (btpb.BigtableClient, *grpc.ClientConn, error) {
	// Set the number of gRPC channels
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		log.Fatalf("Failed to find default credentials: %v", err)
	}

	tokenSource := oauth.TokenSource{TokenSource: creds.TokenSource}
	credsTransport := credentials.NewTLS(&tls.Config{})
	conn, err := grpc.NewClient(
		"bigtable.googleapis.com:443",
		grpc.WithTransportCredentials(credsTransport),
		grpc.WithPerRPCCredentials(oauth.TokenSource{TokenSource: tokenSource}),
		grpc.WithUserAgent(config.UserAgent),
		grpc.WithBlock(),
		grpc.WithTimeout(10*time.Second),
	)
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	client := btpb.NewBigtableClient(conn)
	// defer conn.Close()
	return client, conn, nil
}

func NewProxy(ctx context.Context, config Config) (*Proxy, error) {
	if config.Version == 0 {
		config.Version = primitive.ProtocolVersion4
	}
	if config.MaxVersion == 0 {
		config.MaxVersion = primitive.ProtocolVersion4
	}
	if config.RetryPolicy == nil {
		config.RetryPolicy = NewDefaultRetryPolicy()
	}

	bigtableClients, adminClients, err := createBigtableConnection(ctx, config)
	if err != nil {
		return nil, err
	}

	logger := proxycore.GetOrCreateNopLogger(config.Logger)
	sqlClient, grpcConn, err := createBigtableSqlConnection(ctx, config)
	if err != nil {
		return nil, err
	}
	instanceIDs := strings.Split(config.BigtableConfig.InstanceID, ",")

	tableMetadata := make(map[string]map[string]map[string]*schemaMapping.Column)
	pkMetadata := make(map[string]map[string][]schemaMapping.Column)
	bigtableCl := bigtableModule.NewBigtableClient(bigtableClients, adminClients, logger, sqlClient, config.BigtableConfig, &responsehandler.TypeHandler{}, grpcConn, &schemaMapping.SchemaMappingConfig{})
	for _, v := range instanceIDs {
		instanceID := strings.TrimSpace(v)

		tbdata, pkdata, err := bigtableCl.GetSchemaMappingConfigs(ctx, instanceID, config.BigtableConfig.SchemaMappingTable)
		if err != nil {
			return nil, err
		}
		tableMetadata[instanceID] = tbdata
		pkMetadata[instanceID] = pkdata
	}
	schemaMappingConfig := &schemaMapping.SchemaMappingConfig{
		Logger:             config.Logger,
		TablesMetaData:     tableMetadata,
		PkMetadataCache:    pkMetadata,
		SystemColumnFamily: config.BigtableConfig.DefaultColumnFamily,
	}
	responseHandler := &responsehandler.TypeHandler{
		Logger:              config.Logger,
		SchemaMappingConfig: schemaMappingConfig,
	}

	bigtableCl.LoadConfigs(responseHandler, schemaMappingConfig)
	proxyTranslator := &translator.Translator{
		Logger: config.Logger,
		// todo remove once we support ordered code ints
		EncodeIntValuesWithBigEndian: encodeIntValuesWithBigEndian,
		SchemaMappingConfig:          schemaMappingConfig,
	}

	// Enable OpenTelemetry traces by setting environment variable GOOGLE_API_GO_EXPERIMENTAL_TELEMETRY_PLATFORM_TRACING to the case-insensitive value "opentelemetry" before loading the client library.
	otelConfig := &otelgo.OTelConfig{}
	otelInst := &otelgo.OpenTelemetry{Config: &otelgo.OTelConfig{OTELEnabled: false}}

	var shutdownOTel func(context.Context) error
	// Initialize OpenTelemetry
	if config.OtelConfig.Enabled {
		otelConfig = &otelgo.OTelConfig{
			TracerEndpoint:     config.OtelConfig.Traces.Endpoint,
			MetricEndpoint:     config.OtelConfig.Metrics.Endpoint,
			ServiceName:        config.OtelConfig.ServiceName,
			OTELEnabled:        config.OtelConfig.Enabled,
			TraceSampleRatio:   config.OtelConfig.Traces.SamplingRatio,
			Instance:           config.BigtableConfig.InstanceID,
			HealthCheckEnabled: config.OtelConfig.HealthCheck.Enabled,
			HealthCheckEp:      config.OtelConfig.HealthCheck.Endpoint,
			ServiceVersion:     config.Version.String(),
		}
		config.Logger.Info("OTEL enabled at the application start for the database: " + config.BigtableConfig.InstanceID)
	} else {
		otelConfig = &otelgo.OTelConfig{OTELEnabled: false}
	}
	otelInst, shutdownOTel, err = otelgo.NewOpenTelemetry(ctx, otelConfig, config.Logger)
	if err != nil {
		config.Logger.Error("Failed to enable the OTEL for the database: " + config.BigtableConfig.InstanceID)
		return nil, err
	}
	systemQueryMetadataCache, err := ConstructSystemMetadataRows(tableMetadata)
	if err != nil {
		return nil, err
	}

	proxy := Proxy{
		ctx:                      ctx,
		config:                   config,
		logger:                   logger,
		clients:                  make(map[*client]struct{}),
		listeners:                make(map[*net.Listener]struct{}),
		closed:                   make(chan struct{}),
		bClient:                  bigtableCl,
		translator:               proxyTranslator,
		schemaMapping:            schemaMappingConfig,
		systemQueryMetadataCache: systemQueryMetadataCache,
		otelInst:                 otelInst,
		otelShutdown:             shutdownOTel,
	}
	return &proxy, nil
}

func (p *Proxy) Connect() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isConnected {
		return ErrProxyAlreadyConnected
	}

	var err error
	p.preparedCache, err = getOrCreateDefaultPreparedCache(p.config.PreparedCache)
	if err != nil {
		return fmt.Errorf("unable to create prepared cache %w", err)
	}

	//  connecting to cassandra cluster
	p.cluster, err = proxycore.ConnectCluster(p.ctx, proxycore.ClusterConfig{
		Version: p.config.Version,
		Logger:  p.logger,
	})

	if err != nil {
		return fmt.Errorf("unable to connect to cluster %w", err)
	}

	err = p.buildNodes()
	if err != nil {
		return fmt.Errorf("unable to build node information: %w", err)
	}

	p.buildLocalRow()

	// Create cassandra session
	sess, err := proxycore.ConnectSession(p.ctx, p.cluster, proxycore.SessionConfig{
		Version:       p.cluster.NegotiatedVersion,
		PreparedCache: p.preparedCache,
		Logger:        p.logger,
	})

	if err != nil {
		return fmt.Errorf("unable to connect session %w", err)
	}

	p.sessions[p.cluster.NegotiatedVersion].Store("", sess) // No keyspace

	p.isConnected = true
	return nil
}

// Serve the proxy using the specified listener. It can be called multiple times with different listeners allowing
// them to share the same backend clusters.
func (p *Proxy) Serve(l net.Listener) (err error) {
	l = &closeOnceListener{Listener: l}
	defer l.Close()

	if err = p.addListener(&l); err != nil {
		return err
	}
	defer p.removeListener(&l)

	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-p.closed:
				return ErrProxyClosed
			default:
				return err
			}
		}
		p.handle(conn)
	}
}

func (p *Proxy) addListener(l *net.Listener) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isClosing {
		return ErrProxyClosed
	}
	if !p.isConnected {
		return ErrProxyNotConnected
	}
	p.listeners[l] = struct{}{}
	return nil
}

func (p *Proxy) removeListener(l *net.Listener) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.listeners, l)
}

func (p *Proxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.closed:
	default:
		close(p.closed)
	}
	var err error
	for l := range p.listeners {
		if closeErr := (*l).Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	for cl := range p.clients {
		_ = cl.conn.Close()
		p.eventClients.Delete(cl)
		delete(p.clients, cl)
	}

	p.bClient.Close()
	if p.otelShutdown != nil {
		err = p.otelShutdown(p.ctx)
	}
	return err
}

func (p *Proxy) Ready() bool {
	return true
}

type UCred struct {
	Pid int32
	Uid uint32
}

func getUdsPeerCredentials(conn *net.UnixConn) (UCred, error) {
    return getUdsPeerCredentialsOS(conn)
}

func (p *Proxy) handle(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetKeepAlive(false); err != nil {
			p.logger.Warn("failed to disable keepalive on connection", zap.Error(err))
		}
		if err := tcpConn.SetNoDelay(true); err != nil {
			p.logger.Warn("failed to set TCP_NODELAY on connection", zap.Error(err))
		}
	}

	if unixConn, ok := conn.(*net.UnixConn); ok {
		UCred, err := getUdsPeerCredentials(unixConn)
		if (err != nil || p.config.ClientPid != UCred.Pid || p.config.ClientUid != UCred.Uid) {
			conn.Close()
			p.logger.Error("failed to authenticate connection")
		}
	}

	cl := &client{
		ctx:                 p.ctx,
		proxy:               p,
		preparedSystemQuery: make(map[[preparedIdSize]byte]interface{}),
		preparedQuerys:      cache,
	}
	cl.sender = cl
	p.addClient(cl)
	cl.conn = proxycore.NewConn(conn, cl)
	cl.conn.Start()
}

var (
	schemaVersion, _ = primitive.ParseUuid("4f2b29e6-59b5-4e2d-8fd6-01e32e67f0d7")
)

func (p *Proxy) buildNodes() (err error) {

	localDC := p.config.DC
	if len(localDC) == 0 {
		localDC = p.cluster.Info.LocalDC
		p.logger.Info("no local DC configured using DC from the first successful contact point",
			zap.String("dc", localDC))
	}

	p.localNode = &node{
		dc: localDC,
	}

	return nil
}

func (p *Proxy) buildLocalRow() {
	p.systemLocalValues = map[string]message.Column{
		"key":                     p.encodeTypeFatal(datatype.Varchar, "local"),
		"data_center":             p.encodeTypeFatal(datatype.Varchar, p.localNode.dc),
		"rack":                    p.encodeTypeFatal(datatype.Varchar, "rack1"),
		"tokens":                  p.encodeTypeFatal(datatype.NewListType(datatype.Varchar), [1]string{"-9223372036854775808"}),
		"release_version":         p.encodeTypeFatal(datatype.Varchar, p.config.ReleaseVersion),
		"partitioner":             p.encodeTypeFatal(datatype.Varchar, p.config.Partitioner),
		"cluster_name":            p.encodeTypeFatal(datatype.Varchar, "cql-proxy"),
		"cql_version":             p.encodeTypeFatal(datatype.Varchar, p.config.CQLVersion),
		"schema_version":          p.encodeTypeFatal(datatype.Uuid, schemaVersion), // TODO: Make this match the downstream cluster(s)
		"native_protocol_version": p.encodeTypeFatal(datatype.Varchar, p.config.Version.String()),
		"dse_version":             p.encodeTypeFatal(datatype.Varchar, p.cluster.Info.DSEVersion),
	}
}

func (p *Proxy) encodeTypeFatal(dt datatype.DataType, val interface{}) []byte {
	encoded, err := proxycore.EncodeType(dt, p.config.Version, val)
	if err != nil {
		p.logger.Fatal("unable to encode type", zap.Error(err))
	}
	return encoded
}

// isIdempotent checks whether a prepared ID is idempotent.
// If the proxy receives a query that it's never prepared then this will also return false.
func (p *Proxy) IsIdempotent(id []byte) bool {
	if val, ok := p.preparedIdempotence.Load(preparedIdKey(id)); !ok {
		// This should only happen if the proxy has never had a "PREPARE" request for this query ID.
		p.logger.Error("unable to determine if prepared statement is idempotent",
			zap.String("preparedID", hex.EncodeToString(id)))
		return false
	} else {
		return val.(bool)
	}
}

// MaybeStorePreparedIdempotence stores the idempotence of a "PREPARE" request's query.
// This information is used by future "EXECUTE" requests when they need to be retried.
func (p *Proxy) MaybeStorePreparedIdempotence(raw *frame.RawFrame, msg message.Message) {
	if prepareMsg, ok := msg.(*message.Prepare); ok && raw.Header.OpCode == primitive.OpCodeResult { // Prepared result
		frm, err := codec.ConvertFromRawFrame(raw)
		if err != nil {
			p.logger.Error("error attempting to decode prepared result message")
		} else if _, ok = frm.Body.Message.(*message.PreparedResult); !ok { // TODO: Use prepared type data to disambiguate idempotency
			p.logger.Error("expected prepared result message, but got something else")
		} else {
			idempotent, err := parser.IsQueryIdempotent(prepareMsg.Query)
			if err != nil {
				p.logger.Error("error parsing query for idempotence", zap.Error(err))
			} else if result, ok := frm.Body.Message.(*message.PreparedResult); ok {
				p.preparedIdempotence.Store(preparedIdKey(result.PreparedQueryId), idempotent)
			} else {
				p.logger.Error("expected prepared result, but got some other type of message",
					zap.Stringer("type", reflect.TypeOf(frm.Body.Message)))
			}
		}
	}
}

func (p *Proxy) addClient(cl *client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clients[cl] = struct{}{}
}

func (p *Proxy) registerForEvents(cl *client) {
	p.eventClients.Store(cl, struct{}{})
}

func (p *Proxy) removeClient(cl *client) {
	p.eventClients.Delete(cl)

	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.clients, cl)

}

type Sender interface {
	Send(hdr *frame.Header, msg message.Message)
}

var (
	cache     = make(map[[16]byte]interface{})
	cacheLock = sync.RWMutex{}
)

type client struct {
	ctx                 context.Context
	proxy               *Proxy
	conn                *proxycore.Conn
	keyspace            string
	preparedSystemQuery map[[16]byte]interface{}
	preparedQuerys      map[[16]byte]interface{}
	sender              Sender
}

type PreparedQuery struct {
	Query           string
	SelectedColumns []string
	PreparedColumns []string
}

type UsePreparedQuery struct {
	Query string
	// keyspace string
}

func (c *client) AddQueryToCache(id [16]byte, parsedQueryMeta interface{}) {
	cacheLock.Lock() // Lock the cache for writing.
	c.preparedQuerys[id] = parsedQueryMeta
	cacheLock.Unlock() // Unlock the cache after writing.
}

func (c *client) GetQueryFromCache(id [16]byte) (interface{}, bool) {
	cacheLock.Lock() // Lock the cache for reading.
	parsedQueryMeta, exists := c.preparedQuerys[id]
	cacheLock.Unlock() // Unlock the cache after reading.
	return parsedQueryMeta, exists
}

func (c *client) Receive(reader io.Reader) error {
	raw, err := codec.DecodeRawFrame(reader)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			c.proxy.logger.Error("unable to decode frame", zap.Error(err))
		}
		return err
	}

	if raw.Header.Version > c.proxy.config.MaxVersion || raw.Header.Version < primitive.ProtocolVersion3 {
		c.sender.Send(raw.Header, &message.ProtocolError{
			ErrorMessage: fmt.Sprintf("Invalid or unsupported protocol version %d", raw.Header.Version),
		})
		return nil
	}

	body, err := codec.DecodeBody(raw.Header, bytes.NewReader(raw.Body))
	if err != nil {
		c.proxy.logger.Error("unable to decode body", zap.Error(err))
		return err
	}

	switch msg := body.Message.(type) {
	case *message.Options:
		// CC - responding with status READY
		c.sender.Send(raw.Header, &message.Supported{Options: map[string][]string{
			"CQL_VERSION": {c.proxy.config.CQLVersion},
			"COMPRESSION": {},
		}})
	case *message.Startup:
		// CC -  register for Event types and respond READY
		c.sender.Send(raw.Header, &message.Ready{})
	case *message.Register:
		for _, t := range msg.EventTypes {
			if t == primitive.EventTypeSchemaChange {
				c.proxy.registerForEvents(c)
			}
		}
		c.sender.Send(raw.Header, &message.Ready{})
	case *message.Prepare:
		c.proxy.logger.Debug("Prepare block -", zap.String(Query, msg.Query))
		c.handlePrepare(raw, msg)
	case *partialExecute:
		c.handleExecute(raw, msg)
	case *partialQuery:
		c.handleQuery(raw, msg)
	case *partialBatch:
		c.handleBatch(raw, msg)
	default:
		c.sender.Send(raw.Header, &message.ProtocolError{ErrorMessage: "Unsupported operation"})
	}
	return nil
}

// function to execute query on cassandra
func (c *client) handlePrepare(raw *frame.RawFrame, msg *message.Prepare) {
	c.proxy.logger.Debug("handling prepare", zap.String(Query, msg.Query), zap.Int16("stream", raw.Header.StreamId))

	keyspace := c.keyspace
	if len(msg.Keyspace) != 0 {
		keyspace = msg.Keyspace
	}

	handled, stmt, queryType, err := parser.IsQueryHandledWithQueryType(parser.IdentifierFromString(keyspace), msg.Query)
	if handled {
		if err != nil {
			c.proxy.logger.Error("error parsing query to see if it's handled", zap.String(Query, msg.Query), zap.Error(err))
			c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
		} else {
			switch s := stmt.(type) {
			// handling select statement
			case *parser.SelectStatement:
				if systemColumns, ok := parser.SystemColumnsByName[s.Table]; ok {
					if columns, err := parser.FilterColumns(s, systemColumns); err != nil {
						c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
					} else {
						id := md5.Sum([]byte(msg.Query + keyspace))
						c.sender.Send(raw.Header, &message.PreparedResult{
							PreparedQueryId: id[:],
							ResultMetadata: &message.RowsMetadata{
								ColumnCount: int32(len(columns)),
								Columns:     columns,
							},
						})
						c.preparedSystemQuery[id] = stmt
					}
				} else {
					c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: "system columns doesn't exist"})
				}
				// Prepare Use statement
			case *parser.UseStatement:
				id := md5.Sum([]byte(msg.Query))
				c.preparedSystemQuery[id] = stmt
				c.sender.Send(raw.Header, &message.PreparedResult{
					PreparedQueryId: id[:],
				})
			default:
				c.sender.Send(raw.Header, &message.ServerError{ErrorMessage: "Proxy attempted to intercept an unhandled query"})
			}
		}

	} else {
		c.handleServerPreparedQuery(raw, msg, queryType)
	}
}

// Check if query is already prepared and return response accordingly without re-processing
// Params: id [16]byte - Unique identifier for the prepared statement.
// Returns: []*message.ColumnMetadata - Variable metadata, []*message.ColumnMetadata - Column metadata, bool - Exists in cache.
// Retrieves and returns metadata for Select, Insert, Delete, and Update query maps from the cache, or nil and false if not found.
func (c *client) getMetadataFromCache(id [16]byte) ([]*message.ColumnMetadata, []*message.ColumnMetadata, bool) {
	preparedStmt, found := c.GetQueryFromCache(id)
	if !found {
		return nil, nil, false
	}
	switch st := preparedStmt.(type) {
	case *translator.SelectQueryMap:
		return st.VariableMetadata, st.ReturnMetadata, true
	case *translator.InsertQueryMapping:
		return st.VariableMetadata, st.ReturnMetadata, true
	case *translator.DeleteQueryMapping:
		return st.VariableMetadata, st.ReturnMetadata, true
	case *translator.UpdateQueryMapping:
		return st.VariableMetadata, st.ReturnMetadata, true
	default:
		return nil, nil, false
	}

}

// handleServerPreparedQuery handle prepared query that was supposed to run on cassandra server
// This method will keep track of prepared query in a map and send hashed query_id with result
// metadata and variable column metadata to the client
//
// Parameters:
//   - raw: *frame.RawFrame
//   - msg: *message.Prepare
//
// Returns: nil
func (c *client) handleServerPreparedQuery(raw *frame.RawFrame, msg *message.Prepare, queryType string) {
	var PkIndices []uint16
	var err error
	var columns, variableColumnMetadata []*message.ColumnMetadata

	// Generating unique prepared query_id
	id := md5.Sum([]byte(msg.Query + c.keyspace))
	variableColumnMetadata, columns, found := c.getMetadataFromCache(id)
	if !found {
		switch queryType {
		case selectType:
			columns, variableColumnMetadata, err = c.prepareSelectType(raw, msg, id)
		case insertType:
			columns, variableColumnMetadata, err = c.prepareInsertType(raw, msg, id)
		case deleteType:
			columns, variableColumnMetadata, err = c.prepareDeleteType(raw, msg, id)
		case updateType:
			columns, variableColumnMetadata, err = c.prepareUpdateType(raw, msg, id)
		default:
			c.proxy.logger.Error("Unhandled Prepared Query Scenario", zap.String(Query, msg.Query))
			c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: "Unhandled Prepared Query Scenario"})
			return
		}

		if err != nil {
			return
		}
	}

	// Generating Index array of size of variableMetadata
	for i := range variableColumnMetadata {
		PkIndices = append(PkIndices, uint16(i))
	}

	c.sender.Send(raw.Header, &message.PreparedResult{
		PreparedQueryId: id[:],
		ResultMetadata: &message.RowsMetadata{
			ColumnCount: int32(len(columns)),
			Columns:     columns,
		},
		VariablesMetadata: &message.VariablesMetadata{
			PkIndices: PkIndices,
			Columns:   variableColumnMetadata,
		},
	})

}

// function to handle and delete query of prepared type
func (c *client) prepareDeleteType(raw *frame.RawFrame, msg *message.Prepare, id [16]byte) ([]*message.ColumnMetadata, []*message.ColumnMetadata, error) {
	var returnColumns, variableColumns, columnsWithInOp []string
	var err error

	deleteQueryMetadata, err := c.proxy.translator.TranslateDeleteQuerytoBigtable(msg.Query, true)
	if err != nil {
		c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
		return nil, nil, err
	}

	if slices.Contains(deleteQueryMetadata.ParamKeys, ts_column) {
		variableColumns = append(variableColumns, ts_column)
	}

	// capturing variable columns name assuming all columns are parameterized
	for _, clause := range deleteQueryMetadata.Clauses {
		variableColumns = append(variableColumns, clause.Column)
		// Capture columns with in operator
		if clause.Operator == "IN" {
			columnsWithInOp = append(columnsWithInOp, clause.Column)
		}
	}

	if len(variableColumns) > 0 {
		//Get column metadata for variable fields
		deleteQueryMetadata.VariableMetadata, err = c.proxy.schemaMapping.GetMetadataForColumns(deleteQueryMetadata.Keyspace, deleteQueryMetadata.Table, variableColumns)
		if err != nil {
			c.proxy.logger.Error(metadataFetchError, zap.String(Query, msg.Query), zap.Error(err))
			c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
			return nil, nil, err
		}
		if deleteQueryMetadata.TimestampInfo.HasUsingTimestamp {
			metadata := message.ColumnMetadata{
				Keyspace: deleteQueryMetadata.Keyspace,
				Table:    deleteQueryMetadata.Table,
				Name:     TimestampColumnName,
				Index:    deleteQueryMetadata.TimestampInfo.Index,
				Type:     datatype.Bigint,
			}
			deleteQueryMetadata.VariableMetadata = append([]*message.ColumnMetadata{&metadata}, deleteQueryMetadata.VariableMetadata...)
		}
	}

	// Modify type to list type as being used with in operator
	for _, columnMeta := range deleteQueryMetadata.VariableMetadata {
		if slices.Contains(columnsWithInOp, columnMeta.Name) {
			columnMeta.Type = datatype.NewListType(columnMeta.Type)
		}
	}

	deleteQueryMetadata.ReturnMetadata, err = c.proxy.schemaMapping.GetMetadataForColumns(deleteQueryMetadata.Keyspace, deleteQueryMetadata.Table, returnColumns)
	if err != nil {
		c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
		return nil, nil, err
	}
	// caching Query info
	c.AddQueryToCache(id, deleteQueryMetadata)

	return deleteQueryMetadata.ReturnMetadata, deleteQueryMetadata.VariableMetadata, err
}

// function to handle and insert query of prepared type
func (c *client) prepareInsertType(raw *frame.RawFrame, msg *message.Prepare, id [16]byte) ([]*message.ColumnMetadata, []*message.ColumnMetadata, error) {
	var returnColumns []string
	insertQueryMetadata, err := c.proxy.translator.TranslateInsertQuerytoBigtable(msg.Query, raw.Header.Version, true)

	if err != nil {
		c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
		return nil, nil, err
	}
	insertQueryMetadata.VariableMetadata, err = c.proxy.schemaMapping.GetMetadataForColumns(insertQueryMetadata.Keyspace, insertQueryMetadata.Table, insertQueryMetadata.ParamKeys)
	insertQueryMetadata.VariableMetadata = getTimestampMetadata(*insertQueryMetadata, insertQueryMetadata.VariableMetadata)
	if err != nil {
		c.proxy.logger.Error(metadataFetchError, zap.String(Query, msg.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
		return nil, nil, err
	}

	insertQueryMetadata.ReturnMetadata, err = c.proxy.schemaMapping.GetMetadataForColumns(insertQueryMetadata.Keyspace, insertQueryMetadata.Table, returnColumns)
	if err != nil {
		c.proxy.logger.Error("error getting column metadata", zap.Error(err))
		c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
		return nil, nil, err
	}
	c.AddQueryToCache(id, insertQueryMetadata)

	return insertQueryMetadata.ReturnMetadata, insertQueryMetadata.VariableMetadata, err
}

// function to handle and select query of prepared type
func (c *client) prepareSelectType(raw *frame.RawFrame, msg *message.Prepare, id [16]byte) ([]*message.ColumnMetadata, []*message.ColumnMetadata, error) {
	var variableColumns, columnsWithInOp []string
	translatedSelectQuery, err := c.proxy.translator.TranslateSelectQuerytoBigtable(msg.Query)
	if err != nil {
		c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
		return nil, nil, err
	}

	// Get Column metadata for the table or selected field
	translatedSelectQuery.ReturnMetadata, err = c.proxy.schemaMapping.GetMetadataForSelectedColumns(translatedSelectQuery.Table, translatedSelectQuery.ColumnMeta.Column, translatedSelectQuery.Keyspace)
	if err != nil {
		c.proxy.logger.Error(metadataFetchError, zap.String(Query, msg.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
		return nil, nil, err
	}

	// Capturing variable columns name assuming all columns are parameterized
	for _, clause := range translatedSelectQuery.Clauses {
		variableColumns = append(variableColumns, clause.Column)
		if clause.Operator == "IN" {
			columnsWithInOp = append(columnsWithInOp, clause.Column)
		}
	}

	if slices.Contains(translatedSelectQuery.ParamKeys, limitValue) {
		variableColumns = append(variableColumns, limitValue)
	}

	c.proxy.logger.Debug("Prepare Select Query ", zap.Strings("variableColumns", variableColumns))

	if len(variableColumns) > 0 {
		// Get column metadata for variable fields
		translatedSelectQuery.VariableMetadata, err = c.proxy.schemaMapping.GetMetadataForColumns(translatedSelectQuery.Keyspace, translatedSelectQuery.Table, variableColumns)
		if err != nil {
			c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
			c.proxy.logger.Error(metadataFetchError, zap.String(Query, msg.Query), zap.Error(err))
			return nil, nil, err
		}
	}

	// Modify type to list type as being used with in operator
	for _, columnMeta := range translatedSelectQuery.VariableMetadata {
		if slices.Contains(columnsWithInOp, columnMeta.Name) {
			columnMeta.Type = datatype.NewListType(columnMeta.Type)
		}
	}

	// Caching Query info
	c.AddQueryToCache(id, translatedSelectQuery)
	return translatedSelectQuery.ReturnMetadata, translatedSelectQuery.VariableMetadata, err
}

// function to handle update query of prepared type
func (c *client) prepareUpdateType(raw *frame.RawFrame, msg *message.Prepare, id [16]byte) ([]*message.ColumnMetadata, []*message.ColumnMetadata, error) {
	var returnColumns, variableColumns, columnsWithInOp []string
	var err error

	updateQueryMetadata, err := c.proxy.translator.TranslateUpdateQuerytoBigtable(msg.Query, true)
	if err != nil {
		c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
		return nil, nil, err
	}

	// capturing variable columns name assuming all columns are parameterized
	for _, sets := range updateQueryMetadata.UpdateSetValues {
		if sets.Value == commitTsFn {
			continue
		}
		variableColumns = append(variableColumns, sets.Column)
	}

	// capturing variable columns name assuming all columns are parameterized
	for _, clause := range updateQueryMetadata.Clauses {
		if ts_column == clause.Column {
			continue
		}
		variableColumns = append(variableColumns, clause.Column)
		if clause.Operator == "IN" {
			columnsWithInOp = append(columnsWithInOp, clause.Column)
		}
	}

	if len(variableColumns) > 0 {
		//Get column metadata for variable fields
		updateQueryMetadata.VariableMetadata, err = c.proxy.schemaMapping.GetMetadataForColumns(updateQueryMetadata.Keyspace, updateQueryMetadata.Table, variableColumns)
		updateQueryMetadata.VariableMetadata = getTimestampMetadataForUpdate(*updateQueryMetadata, updateQueryMetadata.VariableMetadata)
		if err != nil {
			c.proxy.logger.Error(metadataFetchError, zap.String(Query, msg.Query), zap.Error(err))
			c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
			return nil, nil, err
		}
	}

	// Modify type to list type as being used with in operator
	for _, columnMeta := range updateQueryMetadata.VariableMetadata {
		if slices.Contains(columnsWithInOp, columnMeta.Name) {
			columnMeta.Type = datatype.NewListType(columnMeta.Type)
		}
		for k, v := range updateQueryMetadata.ComplexOperation {
			if columnMeta.Name == k && v.ExpectedDatatype != nil {
				columnMeta.Type = v.ExpectedDatatype
			}

		}

	}

	updateQueryMetadata.ReturnMetadata, err = c.proxy.schemaMapping.GetMetadataForColumns(updateQueryMetadata.Keyspace, updateQueryMetadata.Table, returnColumns)
	if err != nil {
		c.proxy.logger.Error(metadataFetchError, zap.String(Query, msg.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
		return nil, nil, err
	}
	// caching Query info
	c.AddQueryToCache(id, updateQueryMetadata)

	return updateQueryMetadata.ReturnMetadata, updateQueryMetadata.VariableMetadata, err
}

// handleExecute for prepared query
func (c *client) handleExecute(raw *frame.RawFrame, msg *partialExecute) {
	ctx := context.Background()
	id := preparedIdKey(msg.queryId)
	if stmt, ok := c.preparedSystemQuery[id]; ok {
		c.interceptSystemQuery(raw.Header, stmt)
	} else if preparedStmt, ok := c.GetQueryFromCache(id); ok {
		switch st := preparedStmt.(type) {
		case *translator.SelectQueryMap:
			c.handleExecuteForSelect(raw, msg, st, ctx)
		case *translator.InsertQueryMapping:
			c.handleExecuteForInsert(raw, msg, st, ctx)
		case *translator.DeleteQueryMapping:
			c.handleExecuteForDelete(raw, msg, st, ctx)
		case *translator.UpdateQueryMapping:
			c.handleExecuteForUpdate(raw, msg, st, ctx)
		default:
			c.proxy.logger.Error("Unhandled Prepare Execute Scenario")
			c.sender.Send(raw.Header, &message.ServerError{ErrorMessage: "Unhandled Prepared Query Object"})
		}
	} else {
		c.proxy.logger.Error(unhandledScenario)
		c.sender.Send(raw.Header, &message.ServerError{ErrorMessage: unhandledScenario})
	}
}

// handle batch queries
func (c *client) handleBatch(raw *frame.RawFrame, msg *partialBatch) {
	startTime := time.Now()
	var keySpace string
	var batchQueriesString []string

	otelCtx, span := c.proxy.otelInst.StartSpan(c.proxy.ctx, handleBatch, []attribute.KeyValue{
		attribute.Int("Batch Size", len(msg.queryOrIds)),
	})
	defer c.proxy.otelInst.EndSpan(span)
	var otelErr error
	defer c.proxy.otelInst.RecordMetrics(otelCtx, handleBatch, startTime, handleBatch, otelErr)
	tableMutationsMap := make(map[string][]bigtableModule.MutationData)
	for index, queryId := range msg.queryOrIds {
		queryOrId, ok := queryId.([]byte)
		if !ok {
			otelErr = fmt.Errorf("item is not of type [16]byte")
			c.proxy.otelInst.RecordError(span, otelErr)
			c.proxy.logger.Error("Item is not of type [16]byte")
			continue
		}
		id := preparedIdKey(queryOrId)
		if preparedStmt, ok := c.GetQueryFromCache(id); ok {
			switch st := preparedStmt.(type) {
			case *translator.InsertQueryMapping:
				queryMetadata, mut, err := c.prepareInsertQueryMetadata(raw, msg.BatchPositionalValues[index], st)
				keySpace = st.Keyspace
				if err != nil {
					c.proxy.logger.Error("Error preparing insert batch query metadata", zap.String(Query, st.Query), zap.Error(err))
					c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
					otelErr = err
					c.proxy.otelInst.RecordError(span, otelErr)
					return
				}
				mutationRow := bigtableModule.MutationData{MutationType: "Insert", RowKey: queryMetadata.RowKey, Columns: mut}
				if tableMutationsMap[st.Table] == nil {
					tableMutationsMap[st.Table] = []bigtableModule.MutationData{}
				}
				tableMutationsMap[st.Table] = append(tableMutationsMap[st.Table], mutationRow)
				batchQueriesString = append(batchQueriesString, st.Query)
			case *translator.DeleteQueryMapping:
				keySpace = st.Keyspace
				queryMetadata, err := c.prepareDeleteQueryMetadata(raw, msg.BatchPositionalValues[index], st)
				if err != nil {
					c.proxy.logger.Error("Error preparing delete batch query metadata", zap.String(Query, st.Query), zap.Error(err))
					c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
					otelErr = err
					c.proxy.otelInst.RecordError(span, otelErr)
					return
				}
				mutationRow := bigtableModule.MutationData{MutationType: "Delete", RowKey: queryMetadata.RowKey, Columns: nil}
				if tableMutationsMap[st.Table] == nil {
					tableMutationsMap[st.Table] = []bigtableModule.MutationData{}
				}
				tableMutationsMap[st.Table] = append(tableMutationsMap[st.Table], mutationRow)
				batchQueriesString = append(batchQueriesString, st.Query)
			case *translator.UpdateQueryMapping:
				keySpace = st.Keyspace
				queryMetadata, mutData, err := c.prepareUpdateQueryMetadata(raw, msg.BatchPositionalValues[index], st)
				if err != nil {
					c.proxy.logger.Error("Error preparing updadte batch query metadata", zap.String(Query, st.Query), zap.Error(err))
					c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
					otelErr = err
					c.proxy.otelInst.RecordError(span, otelErr)
					return
				}
				if tableMutationsMap[st.Table] == nil {
					tableMutationsMap[st.Table] = []bigtableModule.MutationData{}
				}
				for _, value := range queryMetadata.DeleteColumnFamilies {
					mutationRow := bigtableModule.MutationData{MutationType: "DeleteColumnFamilies", RowKey: queryMetadata.RowKey, Columns: nil, ColumnFamily: value}
					tableMutationsMap[st.Table] = append(tableMutationsMap[st.Table], mutationRow)
				}
				mutationRow := bigtableModule.MutationData{MutationType: "Update", RowKey: queryMetadata.RowKey, Columns: mutData}
				tableMutationsMap[st.Table] = append(tableMutationsMap[st.Table], mutationRow)
				batchQueriesString = append(batchQueriesString, st.Query)
			default:
				otelErr = fmt.Errorf("unhandled prepared batch query object")
				c.sender.Send(raw.Header, &message.ServerError{ErrorMessage: otelErr.Error()})
				c.proxy.otelInst.RecordError(span, otelErr)
				c.proxy.logger.Error("Unhandled Prepare Batch Scenario")
			}
		} else {
			c.sender.Send(raw.Header, &message.ServerError{ErrorMessage: otelErr.Error()})
			otelErr = fmt.Errorf(errQueryNotPrepared)
			c.proxy.otelInst.RecordError(span, otelErr)
			c.proxy.logger.Error(otelErr.Error())
		}
	}
	c.proxy.logger.Debug("Batch Operation", zap.Strings("Queries", batchQueriesString))
	otelgo.AddAnnotation(otelCtx, sendingBulkApplyMutation)
	var errorMessage string
	hasError := false
	for tableName, mutations := range tableMutationsMap {
		res, err := c.proxy.bClient.ApplyBulkMutation(otelCtx, tableName, mutations, keySpace)
		if err != nil || res.FailedRows != "" {
			c.proxy.otelInst.RecordError(span, err)
			hasError = true
			errorMessage = errorMessage + res.FailedRows + "\n"
		}
	}
	otelgo.AddAnnotation(otelCtx, gotBulkApplyResp)
	if hasError {
		c.sender.Send(raw.Header, &message.ServerError{ErrorMessage: errorMessage})
	}
	c.sender.Send(raw.Header, &message.VoidResult{})
}

// handleExecute for Select prepared query
func (c *client) handleExecuteForSelect(raw *frame.RawFrame, msg *partialExecute, st *translator.SelectQueryMap, ctx context.Context) {
	startTime := time.Now()
	var err error
	var result *message.RowsResult
	params := make(map[string]interface{})

	otelCtx, span := c.proxy.otelInst.StartSpan(ctx, selectType, []attribute.KeyValue{
		attribute.String(cassandraQuery, st.Query),
		attribute.String(rowKey, st.TranslatedQuery),
	})
	defer c.proxy.otelInst.EndSpan(span)
	defer c.proxy.otelInst.RecordMetrics(otelCtx, handleExecuteForSelect, startTime, selectType, err)

	// Get Decoded parameters
	otelgo.AddAnnotation(otelCtx, "Decoding Bytes To Cassandra Column Type")
	for index, columnMetada := range st.VariableMetadata {
		decodedValue, err := utilities.DecodeBytesToCassandraColumnType(msg.PositionalValues[index].Contents, columnMetada.Type, raw.Header.Version)
		if err != nil {
			c.proxy.logger.Error(errorWhileDecoding, zap.String(Query, st.Query), zap.String("Column", columnMetada.Name), zap.Error(err))
			c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
			c.proxy.otelInst.RecordError(span, err)
			return
		}
		params[st.ParamKeys[index]] = decodedValue
	}
	otelgo.AddAnnotation(otelCtx, "Decoding Done")
	query := responsehandler.QueryMetadata{
		Query:               st.TranslatedQuery,
		QueryType:           st.QueryType,
		TableName:           st.Table,
		KeyspaceName:        st.Keyspace,
		ProtocalV:           raw.Header.Version,
		Params:              params,
		SelectedColumns:     st.ColumnMeta.Column,
		AliasMap:            st.AliasMap,
		PrimaryKeys:         st.PrimaryKeys,
		DefaultColumnFamily: c.proxy.translator.SchemaMappingConfig.SystemColumnFamily,
		IsStar:              st.ColumnMeta.Star,
		Limit:               st.Limit,
	}

	query, err = ReplaceLimitValue(query)
	if err != nil {
		c.proxy.logger.Error(errorAtBigtable, zap.String(Query, st.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
		c.proxy.otelInst.RecordError(span, err)
		return
	}
	otelgo.AddAnnotation(otelCtx, executingBigtableSQLAPIRequestEvent)
	result, _, err = c.proxy.bClient.SelectStatement(otelCtx, query)

	otelgo.AddAnnotation(otelCtx, bigtableExecutionDoneEvent)

	if err != nil {
		c.proxy.logger.Error(errorAtBigtable, zap.String(Query, st.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
		c.proxy.otelInst.RecordError(span, err)
		return
	}

	for _, column := range result.Metadata.Columns {
		c.proxy.logger.Info("select type:", zap.String("results", column.Type.String()))
	}
	c.proxy.logger.Info("select results:", zap.Any("results", result))
	c.sender.Send(raw.Header, result)
}

// handleExecute for update prepared query
func (c *client) handleExecuteForUpdate(raw *frame.RawFrame, msg *partialExecute, st *translator.UpdateQueryMapping, ctx context.Context) {
	startTime := time.Now()
	var otelErr error
	otelCtx, span := c.proxy.otelInst.StartSpan(ctx, updateType, []attribute.KeyValue{
		attribute.String(cassandraQuery, st.Query),
		attribute.String(rowKey, st.RowKey),
	})
	defer c.proxy.otelInst.EndSpan(span)
	defer c.proxy.otelInst.RecordMetrics(otelCtx, handleExecuteForUpdate, startTime, updateType, otelErr)

	queryMetadata, _, err := c.prepareUpdateQueryMetadata(raw, msg.PositionalValues, st)

	if err != nil {
		c.proxy.logger.Error("Error preparing update query metadata", zap.String(Query, st.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
		c.proxy.otelInst.RecordError(span, err)
		return
	}
	otelgo.AddAnnotation(otelCtx, executingBigtableRequestEvent)
	resp, err := c.proxy.bClient.UpdateRow(otelCtx, queryMetadata)
	otelgo.AddAnnotation(otelCtx, bigtableExecutionDoneEvent)

	if err != nil {
		c.proxy.logger.Error(errorAtBigtable, zap.String(Query, st.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
		c.proxy.otelInst.RecordError(span, err)
		return
	}
	c.sender.Send(raw.Header, resp)
}

// handleExecute for delete prepared query
func (c *client) handleExecuteForDelete(raw *frame.RawFrame, msg *partialExecute, st *translator.DeleteQueryMapping, ctx context.Context) {
	start := time.Now()
	var otelErr error
	otelCtx, span := c.proxy.otelInst.StartSpan(ctx, deleteType, []attribute.KeyValue{
		attribute.String(cassandraQuery, st.Query),
		attribute.String(rowKey, st.RowKey),
	})
	defer c.proxy.otelInst.EndSpan(span)
	defer c.proxy.otelInst.RecordMetrics(otelCtx, handleExecuteForDelete, start, deleteType, otelErr)

	var deleteMetadata *translator.DeleteQueryMapping
	deleteMetadata, err := c.prepareDeleteQueryMetadata(raw, msg.PositionalValues, st)
	if err != nil {
		c.proxy.logger.Error("Error preparing Delete query metadata", zap.String(Query, st.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
		c.proxy.otelInst.RecordError(span, err)
		return
	}

	var resp *message.RowsResult
	resp, err = c.proxy.bClient.DeleteRowNew(c.proxy.ctx, deleteMetadata)
	if err != nil {
		c.proxy.logger.Error(errorAtBigtable, zap.String(Query, st.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
		c.proxy.otelInst.RecordError(span, err)
		return
	}
	otelgo.AddAnnotation(otelCtx, bigtableExecutionDoneEvent)
	c.sender.Send(raw.Header, resp)
}

// handleExecute for insert prepared query
func (c *client) handleExecuteForInsert(raw *frame.RawFrame, msg *partialExecute, st *translator.InsertQueryMapping, ctx context.Context) {
	start := time.Now()
	queryMetadata, _, err := c.prepareInsertQueryMetadata(raw, msg.PositionalValues, st)
	if err != nil {
		c.proxy.logger.Error("Error preparing insert query metadata", zap.String(Query, st.Query), zap.Error(err))
		c.sender.Send(raw.Header, &message.ConfigError{ErrorMessage: err.Error()})
		return
	}
	invalidColumn := detectEmptyPrimaryKey(queryMetadata)
	if invalidColumn != "" {
		err = fmt.Errorf("invalid null value in condition for column %s", invalidColumn)
	}
	var otelErr error
	otelCtx, span := c.proxy.otelInst.StartSpan(ctx, insertType, []attribute.KeyValue{
		attribute.String(cassandraQuery, queryMetadata.Query),
		attribute.String(rowKey, queryMetadata.RowKey),
	})
	defer c.proxy.otelInst.EndSpan(span)
	defer c.proxy.otelInst.RecordMetrics(otelCtx, handleExecuteForDelete, start, deleteType, otelErr)

	otelgo.AddAnnotation(otelCtx, executingBigtableRequestEvent)
	resp, iErr := c.proxy.bClient.InsertRow(otelCtx, queryMetadata)
	otelgo.AddAnnotation(otelCtx, bigtableExecutionDoneEvent)

	if iErr != nil {
		c.proxy.logger.Error(errorAtBigtable, zap.String(Query, st.Query), zap.Error(iErr))
		c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: iErr.Error()})
		c.proxy.otelInst.RecordError(span, err)
		return
	}
	c.sender.Send(raw.Header, resp)
}

// Prepare update query metadata
func (c *client) prepareUpdateQueryMetadata(raw *frame.RawFrame, paramValues []*primitive.Value, st *translator.UpdateQueryMapping) (*translator.UpdateQueryMapping, []bigtableModule.ColumnData, error) {
	updateData, iErr := c.proxy.translator.BuildUpdatePrepareQuery(st.Columns, paramValues, st, raw.Header.Version)
	if iErr != nil {
		return nil, nil, fmt.Errorf("error building update prepare query:%s", iErr)
	}
	var mutationData []bigtableModule.ColumnData
	for index, value := range updateData.Columns {
		if bv, ok := updateData.Values[index].([]byte); ok {
			mcd := bigtableModule.ColumnData{ColumnFamily: value.ColumnFamily, Name: value.Name, Contents: bv}
			mutationData = append(mutationData, mcd)
		} else {
			c.proxy.logger.Error("Value is not of type []byte", zap.String("column", value.Name))
			return nil, nil, fmt.Errorf("value for column %s is not of type []byte", value.Name)
		}
	}
	return updateData, mutationData, nil
}

// Prepare delete query metadata
func (c *client) prepareDeleteQueryMetadata(raw *frame.RawFrame, paramValue []*primitive.Value, st *translator.DeleteQueryMapping) (*translator.DeleteQueryMapping, error) {
	var variableColumns []string
	variableColumnMetadata := make([]*message.ColumnMetadata, 0)
	for _, clause := range st.Clauses {
		variableColumns = append(variableColumns, clause.Column)
	}

	if len(variableColumns) != 0 {
		var err error
		variableColumnMetadata, err = c.proxy.schemaMapping.GetMetadataForColumns(st.Keyspace, st.Table, variableColumns)
		if err != nil {
			c.proxy.logger.Error(metadataFetchError, zap.String(Query, st.Query), zap.Error(err))
			return nil, fmt.Errorf("%s -> %s", metadataFetchError, err.Error())
		}
	}

	rowKey, timestamp, err := c.proxy.translator.BuildDeletePrepareQuery(paramValue, st, variableColumnMetadata, raw.Header.Version)
	if err != nil {
		return nil, fmt.Errorf("error building rowkey for delete prepare query:%w", err)
	}
	if timestamp.HasUsingTimestamp {
		return nil, fmt.Errorf("error delete prepare query: %s", "delete using timestamp is not allowed")
	}
	c.proxy.logger.Debug("Delete PreparedExecute Query", zap.String("RowKey", rowKey))

	deleteQueryData := &translator.DeleteQueryMapping{
		Query:           st.Query,
		QueryType:       st.QueryType,
		Table:           st.Table,
		Keyspace:        st.Keyspace,
		Clauses:         st.Clauses,
		Params:          st.Params,
		ParamKeys:       st.ParamKeys,
		PrimaryKeys:     st.PrimaryKeys,
		RowKey:          rowKey,
		TimestampInfo:   timestamp,
		SelectedColumns: st.SelectedColumns,
	}
	return deleteQueryData, nil
}

// Prepare insert query metadata
func (c *client) prepareInsertQueryMetadata(raw *frame.RawFrame, paramValue []*primitive.Value, st *translator.InsertQueryMapping) (*translator.InsertQueryMapping, []bigtableModule.ColumnData, error) {
	insertData, iErr := c.proxy.translator.BuildInsertPrepareQuery(st.Columns, paramValue, st, raw.Header.Version)
	if iErr != nil {
		return nil, nil, fmt.Errorf("error building insert prepare query:%s", iErr)
	}
	c.proxy.logger.Debug("Insert PreparedExecute Query", zap.String("TranslatedQuery", "Insert Operation use mutation"))

	var mutationData []bigtableModule.ColumnData
	for index, value := range insertData.Columns {
		if bv, ok := insertData.Values[index].([]byte); ok {
			mcd := bigtableModule.ColumnData{ColumnFamily: value.ColumnFamily, Name: value.Name, Contents: bv}
			mutationData = append(mutationData, mcd)
		} else {
			c.proxy.logger.Error("Value is not of type []byte", zap.String("column", value.Name))
			return nil, nil, fmt.Errorf("value for column %s is not of type []byte", value.Name)
		}

	}
	return insertData, mutationData, nil
}

func (c *client) handleQuery(raw *frame.RawFrame, msg *partialQuery) {
	startTime := time.Now()
	c.proxy.logger.Debug("handling query", zap.String("encodedQuery", msg.query), zap.Int16("stream", raw.Header.StreamId))

	handled, stmt, queryType, err := parser.IsQueryHandledWithQueryType(parser.IdentifierFromString(c.keyspace), msg.query)
	otelCtx, span := c.proxy.otelInst.StartSpan(c.proxy.ctx, handleQuery, []attribute.KeyValue{
		attribute.String("Query", msg.query),
	})
	defer c.proxy.otelInst.EndSpan(span)
	if handled {
		if err != nil {
			c.proxy.logger.Error("error parsing query to see if it's handled", zap.String(Query, msg.query), zap.Error(err))
			c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
			c.proxy.otelInst.RecordError(span, err)
			return
		} else {
			c.interceptSystemQuery(raw.Header, stmt)
		}
	} else {
		var result *message.RowsResult
		var otelErr error
		defer c.proxy.otelInst.RecordMetrics(otelCtx, handleQuery, startTime, queryType, otelErr)

		switch queryType {
		case selectType:
			translatedSelectQuery, err := c.proxy.translator.TranslateSelectQuerytoBigtable(msg.query)
			if err != nil {
				c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			}
			isGroupBy := false
			if len(translatedSelectQuery.GroupByColumns) > 0 {
				isGroupBy = true
			}
			queryMeta := responsehandler.QueryMetadata{
				Query:               translatedSelectQuery.TranslatedQuery,
				TableName:           translatedSelectQuery.Table,
				KeyspaceName:        translatedSelectQuery.Keyspace,
				ProtocalV:           raw.Header.Version,
				Params:              translatedSelectQuery.Params,
				SelectedColumns:     translatedSelectQuery.ColumnMeta.Column,
				PrimaryKeys:         translatedSelectQuery.PrimaryKeys,
				AliasMap:            translatedSelectQuery.AliasMap,
				DefaultColumnFamily: c.proxy.translator.SchemaMappingConfig.SystemColumnFamily,
				IsStar:              translatedSelectQuery.ColumnMeta.Star,
				IsGroupBy:           isGroupBy,
			}

			otelgo.AddAnnotation(otelCtx, executingBigtableSQLAPIRequestEvent)
			result, _, err = c.proxy.bClient.SelectStatement(otelCtx, queryMeta)
			otelgo.AddAnnotation(otelCtx, bigtableExecutionDoneEvent)

			if err != nil {
				c.proxy.logger.Error(errorAtBigtable, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			}
			c.sender.Send(raw.Header, result)
			return

		case insertType:
			insertData, err := c.proxy.translator.TranslateInsertQuerytoBigtable(msg.query, raw.Header.Version, false)
			if err != nil {
				c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			}

			otelgo.AddAnnotation(otelCtx, executingBigtableRequestEvent)
			resp, err := c.proxy.bClient.InsertRow(otelCtx, insertData)
			otelgo.AddAnnotation(otelCtx, bigtableExecutionDoneEvent)

			if err != nil {
				c.proxy.logger.Error(errorAtBigtable, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			}
			c.proxy.logger.Info("Data inserted successfully")
			if !insertData.IfNotExists {
				c.sender.Send(raw.Header, &message.VoidResult{})
				return
			}
			c.sender.Send(raw.Header, resp)
			return
		case dropType:
			queryMetadata, err := c.proxy.translator.TranslateDropTableToBigtable(msg.query)
			if err != nil {
				c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			}

			otelgo.AddAnnotation(otelCtx, executingBigtableRequestEvent)
			err = c.proxy.bClient.DropTable(c.proxy.ctx, queryMetadata, c.proxy.config.BigtableConfig.SchemaMappingTable)
			otelgo.AddAnnotation(otelCtx, bigtableExecutionDoneEvent)

			if err != nil {
				c.proxy.logger.Error(errorAtBigtable, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			} else {
				c.sender.Send(raw.Header, &message.VoidResult{})
				return
			}

		case createType:
			queryMetadata, err := c.proxy.translator.TranslateCreateTableToBigtable(msg.query)
			if err != nil {
				c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			}

			otelgo.AddAnnotation(otelCtx, executingBigtableRequestEvent)
			err = c.proxy.bClient.CreateTable(c.proxy.ctx, queryMetadata, c.proxy.config.BigtableConfig.SchemaMappingTable)
			otelgo.AddAnnotation(otelCtx, bigtableExecutionDoneEvent)

			if err != nil {
				c.proxy.logger.Error(errorAtBigtable, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			} else {
				c.sender.Send(raw.Header, &message.VoidResult{})
				return
			}

		case alterType:
			queryMetadata, err := c.proxy.translator.TranslateAlterTableToBigtable(msg.query)
			if err != nil {
				c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			}

			otelgo.AddAnnotation(otelCtx, executingBigtableRequestEvent)
			err = c.proxy.bClient.AlterTable(c.proxy.ctx, queryMetadata, c.proxy.config.BigtableConfig.SchemaMappingTable)
			otelgo.AddAnnotation(otelCtx, bigtableExecutionDoneEvent)

			if err != nil {
				c.proxy.logger.Error(errorAtBigtable, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			} else {
				c.sender.Send(raw.Header, &message.VoidResult{})
				return
			}

		case deleteType:
			queryMetadata, err := c.proxy.translator.TranslateDeleteQuerytoBigtable(msg.query, false)
			if err != nil {
				c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			}
			if queryMetadata.TimestampInfo.HasUsingTimestamp {
				e := errors.New("delete using timestamp is not allowed")
				c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.query), zap.Error(e))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: e.Error()})
				return
			}

			var dErr error
			otelgo.AddAnnotation(otelCtx, bigtableExecutionDoneEvent)
			result, dErr = c.proxy.bClient.DeleteRowNew(c.proxy.ctx, queryMetadata)
			if dErr != nil {
				c.proxy.logger.Error(errorAtBigtable, zap.String(Query, msg.query), zap.Error(dErr))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: dErr.Error()})
				otelErr = dErr
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			} else {
				if !queryMetadata.IfExists {
					c.sender.Send(raw.Header, &message.VoidResult{})
					return
				}
				c.sender.Send(raw.Header, result)
				return
			}

		case updateType:
			updateQueryMetaData, err := c.proxy.translator.TranslateUpdateQuerytoBigtable(msg.query, false)
			if err != nil {
				c.proxy.logger.Error(translatorErrorMessage, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			}

			otelgo.AddAnnotation(otelCtx, executingBigtableRequestEvent)
			resp, err := c.proxy.bClient.UpdateRow(otelCtx, updateQueryMetaData)
			otelgo.AddAnnotation(otelCtx, bigtableExecutionDoneEvent)

			if err != nil {
				c.proxy.logger.Error(errorAtBigtable, zap.String(Query, msg.query), zap.Error(err))
				c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: err.Error()})
				otelErr = err
				c.proxy.otelInst.RecordError(span, otelErr)
				return
			}
			c.proxy.logger.Info("Data Updated successfully")
			if !updateQueryMetaData.IfExists {
				c.sender.Send(raw.Header, &message.VoidResult{})
				return
			}
			c.sender.Send(raw.Header, resp)
			return

		default:
			otelErr = fmt.Errorf("invalid query type: %s", queryType)
			c.proxy.otelInst.RecordError(span, otelErr)
			c.proxy.logger.Error(otelErr.Error(), zap.String(Query, msg.query))
			c.sender.Send(raw.Header, &message.Invalid{ErrorMessage: otelErr.Error()})
			return
		}
	}
}

func (c *client) filterSystemLocalValues(stmt *parser.SelectStatement, filtered []*message.ColumnMetadata) (row []message.Column, err error) {
	return parser.FilterValues(stmt, filtered, func(name string) (value message.Column, err error) {
		if name == "rpc_address" {
			return proxycore.EncodeType(datatype.Inet, c.proxy.cluster.NegotiatedVersion, net.ParseIP("127.0.0.1"))
		} else if name == "host_id" {
			return proxycore.EncodeType(datatype.Uuid, c.proxy.cluster.NegotiatedVersion, nameBasedUUID("127.0.0.1"))
		} else if val, ok := c.proxy.systemLocalValues[name]; ok {
			return val, nil
		} else if name == parser.CountValueName {
			return encodedOneValue, nil
		} else {
			return nil, fmt.Errorf("no column value for %s", name)
		}
	})
}

func (c *client) filterSystemPeerValues(stmt *parser.SelectStatement, filtered []*message.ColumnMetadata, peer *node, peerCount int) (row []message.Column, err error) {
	return parser.FilterValues(stmt, filtered, func(name string) (value message.Column, err error) {
		if name == "data_center" {
			return proxycore.EncodeType(datatype.Varchar, c.proxy.cluster.NegotiatedVersion, peer.dc)
		} else if name == "host_id" {
			return proxycore.EncodeType(datatype.Uuid, c.proxy.cluster.NegotiatedVersion, nameBasedUUID(peer.addr.String()))
		} else if name == "tokens" {
			return proxycore.EncodeType(datatype.NewListType(datatype.Varchar), c.proxy.cluster.NegotiatedVersion, peer.tokens)
		} else if name == "peer" {
			return proxycore.EncodeType(datatype.Inet, c.proxy.cluster.NegotiatedVersion, peer.addr.IP)
		} else if name == "rpc_address" {
			return proxycore.EncodeType(datatype.Inet, c.proxy.cluster.NegotiatedVersion, peer.addr.IP)
		} else if val, ok := c.proxy.systemLocalValues[name]; ok {
			return val, nil
		} else if name == parser.CountValueName {
			return proxycore.EncodeType(datatype.Int, c.proxy.cluster.NegotiatedVersion, peerCount)
		} else {
			return nil, fmt.Errorf("no column value for %s", name)
		}
	})
}

// getSystemMetadata retrieves system metadata for `system_schema` keyspaces, tables, or columns.
//
// Parameters:
// - hdr: *frame.Header (request version info)
// - s: *parser.SelectStatement (keyspace and table info)
//
// Returns:
// - []message.Row: Metadata rows for the requested table; empty if keyspace/table is invalid.
func (c *client) getSystemMetadata(hdr *frame.Header, s *parser.SelectStatement) ([]message.Row, error) {
	if s.Keyspace != systemSchema || (s.Table != keyspaces && s.Table != tables && s.Table != columns) {
		return nil, nil
	}

	var cache map[primitive.ProtocolVersion][]message.Row
	var errMsg error
	switch s.Table {
	case keyspaces:
		cache = c.proxy.systemQueryMetadataCache.KeyspaceSystemQueryMetadataCache
		errMsg = fmt.Errorf(systemQueryMetadataNotFoundError, "KeyspaceSystemQueryMetadataCache", hdr.Version)
	case tables:
		cache = c.proxy.systemQueryMetadataCache.TableSystemQueryMetadataCache
		errMsg = fmt.Errorf(systemQueryMetadataNotFoundError, "TableSystemQueryMetadataCache", hdr.Version)
	case columns:
		cache = c.proxy.systemQueryMetadataCache.ColumnsSystemQueryMetadataCache
		errMsg = fmt.Errorf(systemQueryMetadataNotFoundError, "ColumnsSystemQueryMetadataCache", hdr.Version)
	}

	if data, exist := cache[hdr.Version]; !exist {
		return nil, errMsg
	} else {
		return data, nil
	}
}

// Intercept and handle system query
func (c *client) interceptSystemQuery(hdr *frame.Header, stmt interface{}) {
	switch s := stmt.(type) {
	case *parser.SelectStatement:
		if s.Keyspace == systemSchema || s.Keyspace == systemVirtualSchema {
			var localColumns []*message.ColumnMetadata
			var isFound bool
			if s.Keyspace == systemSchema {
				localColumns, isFound = parser.SystemSchematablesColumn[s.Table]
				if isFound {
					tableMetadata := &message.RowsMetadata{
						ColumnCount: int32(len(localColumns)),
						Columns:     localColumns,
					}

					data, err := c.getSystemMetadata(hdr, s)
					if err != nil {
						c.sender.Send(hdr, &message.Invalid{ErrorMessage: err.Error()})
						return
					}

					c.sender.Send(hdr, &message.RowsResult{
						Metadata: tableMetadata,
						Data:     data,
					})
					return
				}
			} else {
				// get Table metadata for system_virtual_schema schema
				localColumns, isFound = parser.SystemVirtualSchemaColumn[s.Table]
				if isFound {
					c.sender.Send(hdr, &message.RowsResult{
						Metadata: &message.RowsMetadata{
							ColumnCount: int32(len(localColumns)),
							Columns:     localColumns,
						},
					})
					return
				}
			}
			if !isFound {
				c.sender.Send(hdr, &message.Invalid{ErrorMessage: "Error while fetching mocked table info"})
				return
			}
		} else if s.Table == local {
			localColumns := parser.SystemLocalColumns
			if len(c.proxy.cluster.Info.DSEVersion) > 0 {
				localColumns = parser.DseSystemLocalColumns
			}
			if columns, err := parser.FilterColumns(s, localColumns); err != nil {
				c.sender.Send(hdr, &message.Invalid{ErrorMessage: err.Error()})
			} else if row, err := c.filterSystemLocalValues(s, columns); err != nil {
				c.sender.Send(hdr, &message.Invalid{ErrorMessage: err.Error()})
			} else {
				c.sender.Send(hdr, &message.RowsResult{
					Metadata: &message.RowsMetadata{
						ColumnCount: int32(len(columns)),
						Columns:     columns,
					},
					Data: []message.Row{row},
				})
			}
		} else if s.Table == "peers" {
			peersColumns := parser.SystemPeersColumns
			if len(c.proxy.cluster.Info.DSEVersion) > 0 {
				peersColumns = parser.DseSystemPeersColumns
			}
			if columns, err := parser.FilterColumns(s, peersColumns); err != nil {
				c.sender.Send(hdr, &message.Invalid{ErrorMessage: err.Error()})
			} else {
				var data []message.Row
				for _, n := range c.proxy.nodes {
					if n != c.proxy.localNode {
						var row message.Row
						row, err = c.filterSystemPeerValues(s, columns, n, len(c.proxy.nodes)-1)
						if err != nil {
							break
						}
						data = append(data, row)
					}
				}
				if err != nil {
					c.sender.Send(hdr, &message.Invalid{ErrorMessage: err.Error()})
				} else {
					c.sender.Send(hdr, &message.RowsResult{
						Metadata: &message.RowsMetadata{
							ColumnCount: int32(len(columns)),
							Columns:     columns,
						},
						Data: data,
					})
				}
			}
			// CC- metadata is mocked here as well for system queries
		} else if columns, ok := parser.SystemColumnsByName[s.Table]; ok {
			c.sender.Send(hdr, &message.RowsResult{
				Metadata: &message.RowsMetadata{
					ColumnCount: int32(len(columns)),
					Columns:     columns,
				},
			})
		} else {
			c.sender.Send(hdr, &message.Invalid{ErrorMessage: "Doesn't exist"})
		}
	case *parser.UseStatement:
		c.sender.Send(hdr, &message.SetKeyspaceResult{Keyspace: s.Keyspace})
	default:
		c.sender.Send(hdr, &message.ServerError{ErrorMessage: "Proxy attempted to intercept an unhandled query"})
	}
}

func (c *client) Send(hdr *frame.Header, msg message.Message) {
	_ = c.conn.Write(proxycore.SenderFunc(func(writer io.Writer) error {
		return codec.EncodeFrame(frame.NewFrame(hdr.Version, hdr.StreamId, msg), writer)
	}))
}

func (c *client) Closing(_ error) {
	c.proxy.removeClient(c)
}

func getOrCreateDefaultPreparedCache(cache proxycore.PreparedCache) (proxycore.PreparedCache, error) {
	if cache == nil {
		return NewDefaultPreparedCache(1e8 / 256) // ~100MB with an average query size of 256 bytes
	}
	return cache, nil
}

// NewDefaultPreparedCache creates a new default prepared cache capping the max item capacity to `size`.
func NewDefaultPreparedCache(size int) (proxycore.PreparedCache, error) {
	cache, err := lru.New(size)
	if err != nil {
		return nil, err
	}
	return &defaultPreparedCache{cache}, nil
}

type defaultPreparedCache struct {
	cache *lru.Cache
}

func (d defaultPreparedCache) Store(id string, entry *proxycore.PreparedEntry) {
	d.cache.Add(id, entry)
}

func (d defaultPreparedCache) Load(id string) (entry *proxycore.PreparedEntry, ok bool) {
	if val, ok := d.cache.Get(id); ok {
		return val.(*proxycore.PreparedEntry), true
	}
	return nil, false
}

func preparedIdKey(bytes []byte) [preparedIdSize]byte {
	var buf [preparedIdSize]byte
	copy(buf[:], bytes)
	return buf
}

func nameBasedUUID(name string) primitive.UUID {
	var uuid primitive.UUID
	m := crypto.MD5.New()
	_, _ = io.WriteString(m, name)
	hash := m.Sum(nil)
	for i := 0; i < len(uuid); i++ {
		uuid[i] = hash[i]
	}
	uuid[6] &= 0x0F
	uuid[6] |= 0x30
	uuid[8] &= 0x3F
	uuid[8] |= 0x80
	return uuid
}

// Wrap the listener so that if it's closed in the serve loop it doesn't race with proxy Close()
type closeOnceListener struct {
	net.Listener
	once     sync.Once
	closeErr error
}

func (oc *closeOnceListener) Close() error {
	oc.once.Do(oc.close)
	return oc.closeErr
}

func (oc *closeOnceListener) close() { oc.closeErr = oc.Listener.Close() }

// detectEmptyPrimaryKey checks if any primary key column in an InsertQueryMapping has a nil value.
//
// Parameters:
//   - query: A pointer to an InsertQueryMapping containing the columns and their corresponding values
//     of an insert query. The map includes column definitions that specify whether a column is a primary key.
//
// Returns: A string containing the name of the first primary key column that has a nil value. If all primary
// keys have non-nil values, an empty string is returned. No errors are returned as the function only checks
// for nil values in primary keys without complex processing.
func detectEmptyPrimaryKey(query *translator.InsertQueryMapping) string {
	columns := query.Columns
	values := query.Values
	for index, column := range columns {
		if column.IsPrimaryKey {
			if value, ok := values[index].([]uint8); ok {
				if len(value) == 0 {
					return column.Name
				}
			} else {
				return column.Name
			}
		}
	}
	return ""
}
