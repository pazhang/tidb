// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tikv

import (
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/pd/pd-client"
	"github.com/pingcap/tidb"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/privilege"
	"github.com/pingcap/tidb/store/tikv/mock-tikv"
	"github.com/pingcap/tidb/store/tikv/oracle"
	"github.com/pingcap/tidb/store/tikv/oracle/oracles"
	"github.com/pingcap/tidb/store/tikv/tikvrpc"
	goctx "golang.org/x/net/context"
)

type storeCache struct {
	sync.Mutex
	cache map[string]*tikvStore
}

var mc storeCache

// Driver implements engine Driver.
type Driver struct {
}

// Open opens or creates an TiKV storage with given path.
// Path example: tikv://etcd-node1:port,etcd-node2:port?cluster=1&disableGC=false
func (d Driver) Open(path string) (kv.Storage, error) {
	mc.Lock()
	defer mc.Unlock()

	etcdAddrs, disableGC, err := parsePath(path)
	if err != nil {
		return nil, errors.Trace(err)
	}

	pdCli, err := pd.NewClient(etcdAddrs)
	if err != nil {
		if strings.Contains(err.Error(), "i/o timeout") {
			return nil, errors.Annotate(err, txnRetryableMark)
		}
		return nil, errors.Trace(err)
	}

	// FIXME: uuid will be a very long and ugly string, simplify it.
	uuid := fmt.Sprintf("tikv-%v", pdCli.GetClusterID(goctx.TODO()))
	if store, ok := mc.cache[uuid]; ok {
		return store, nil
	}

	s, err := newTikvStore(uuid, &codecPDClient{pdCli}, newRPCClient(), !disableGC)
	if err != nil {
		return nil, errors.Trace(err)
	}
	s.etcdAddrs = etcdAddrs
	mc.cache[uuid] = s
	return s, nil
}

// MockDriver is in memory mock TiKV driver.
type MockDriver struct {
}

// Open creates a MockTiKV storage.
func (d MockDriver) Open(path string) (kv.Storage, error) {
	u, err := url.Parse(path)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if !strings.EqualFold(u.Scheme, "mocktikv") {
		return nil, errors.Errorf("Uri scheme expected(mocktikv) but found (%s)", u.Scheme)
	}
	return NewMockTikvStore(WithPath(u.Path))
}

// update oracle's lastTS every 2000ms.
var oracleUpdateInterval = 2000

type tikvStore struct {
	clusterID    uint64
	uuid         string
	oracle       oracle.Oracle
	client       Client
	pdClient     pd.Client
	regionCache  *RegionCache
	lockResolver *LockResolver
	gcWorker     *GCWorker
	etcdAddrs    []string
	mock         bool
	enableGC     bool

	safePoint uint64
	spTime    time.Time
	spSession tidb.Session // this is used to obtain safePoint from remote
	spMutex   sync.Mutex   // this is used to update safePoint and spTime
	spMsg     chan string  // this is used to nofity when the store is closed
}

func (s *tikvStore) createSPSession() {
	log.Error("Enter createSPSession!\n")
	for {
		var err error
		s.spSession, err = tidb.CreateSession(s)
		if err != nil {
			log.Warnf("[safepoint] create session err: %v", err)
			continue
		}
		// Disable privilege check for gc worker session.
		privilege.BindPrivilegeManager(s.spSession, nil)
		log.Error("Exit createSPSession!\n")
		return
	}
}

func (s *tikvStore) saveUint64(key string, t uint64) error {
	str := fmt.Sprintf("%v", t)
	err := s.saveValueToSysTable(key, str)
	return errors.Trace(err)
}

func (s *tikvStore) loadUint64(key string) (uint64, error) {
	str, err := s.loadValueFromSysTable(key)
	if err != nil {
		return 0, errors.Trace(err)
	}
	if str == "" {
		log.Error("No Result\n")
		return 0, nil
	}
	t, err := strconv.ParseUint(str, 10, 64)
	if err != nil {
		return 0, errors.Trace(err)
	}
	return t, nil
}

func (s *tikvStore) saveValueToSysTable(key, value string) error {
	stmt := fmt.Sprintf(`INSERT INTO mysql.tidb VALUES ('%[1]s', '%[2]s', '%[3]s')
			       ON DUPLICATE KEY
			       UPDATE variable_value = '%[2]s', comment = '%[3]s'`,
		key, value, gcVariableComments[key])
	_, err := s.spSession.Execute(stmt)
	log.Debugf("[gc worker] save kv, %s:%s %v", key, value, err)
	return errors.Trace(err)
}

func (s *tikvStore) loadValueFromSysTable(key string) (string, error) {
	stmt := fmt.Sprintf(`SELECT (variable_value) FROM mysql.tidb WHERE variable_name='%s'`, key)
	rs, err := s.spSession.Execute(stmt)
	if err != nil {
		return "", errors.New("no value")
	}
	row, err := rs[0].Next()
	if err != nil {
		return "", errors.Trace(err)
	}
	if row == nil {
		log.Debugf("[safepoint] load kv, %s:nil", key)
		return "", nil
	}
	value := row.Data[0].GetString()
	log.Debugf("[safepoint] load kv, %s:%s", key, value)
	return value, nil
}

func (s *tikvStore) CheckVisibility() (uint64, error) {
	s.spMutex.Lock()
	currentSafePoint := s.safePoint
	lastLocalTime := s.spTime
	s.spMutex.Unlock()
	diff := time.Since(lastLocalTime)

	if diff > 100*time.Second {
		return 0, errors.New("start timestamp may fall behind safepoint")
	}

	return currentSafePoint, nil
}

func newTikvStore(uuid string, pdClient pd.Client, client Client, enableGC bool) (*tikvStore, error) {
	o, err := oracles.NewPdOracle(pdClient, time.Duration(oracleUpdateInterval)*time.Millisecond)
	if err != nil {
		return nil, errors.Trace(err)
	}
	_, mock := client.(*mocktikv.RPCClient)
	store := &tikvStore{
		clusterID:   pdClient.GetClusterID(goctx.TODO()),
		uuid:        uuid,
		oracle:      o,
		client:      client,
		pdClient:    pdClient,
		regionCache: NewRegionCache(pdClient),
		mock:        mock,

		safePoint: 0,
		spTime:    time.Now(),
		spMsg:     make(chan string),
	}
	store.lockResolver = newLockResolver(store)
	store.enableGC = enableGC

	return store, nil
}

func (s *tikvStore) EtcdAddrs() []string {
	return s.etcdAddrs
}

// StartGCWorker starts GC worker, it's called in BootstrapSession, don't call this function more than once.
func (s *tikvStore) StartGCWorker() error {
	go func() {
		for {
			log.Error("Create SP Session\n")
			s.createSPSession()

			for {
				repeatbreak := false
				select {
				case <-s.spMsg:
					log.Error("[safepoint store close]\n")
					return
				default:
					log.Error("Start Fetch SafePoint\n")
					newSafePoint, err := s.loadUint64(gcSavedSafePoint)
					if err == nil {
						s.spMutex.Lock()
						s.safePoint = newSafePoint
						s.spTime = time.Now()
						s.spMutex.Unlock()
						log.Error("[safepoint load OK]\n")
					} else {
						s.spMutex.Lock()
						s.spTime = time.Now()
						s.spMutex.Unlock()
						log.Error("[safepoint load error]\n")
						repeatbreak = true
					}
					time.Sleep(5 * time.Second) // this is configurable
				}
				if repeatbreak {
					break
				}
			}
		}
	}()

	if !s.enableGC {
		return nil
	}

	fmt.Printf("Start a gc worker\n")
	gcWorker, err := NewGCWorker(s)
	if err != nil {
		return errors.Trace(err)
	}
	s.gcWorker = gcWorker
	return nil
}

type mockOptions struct {
	cluster        *mocktikv.Cluster
	mvccStore      mocktikv.MVCCStore
	clientHijack   func(Client) Client
	pdClientHijack func(pd.Client) pd.Client
	path           string
}

// MockTiKVStoreOption is used to control some behavior of mock tikv.
type MockTiKVStoreOption func(*mockOptions)

// WithHijackClient hijacks KV client's behavior, makes it easy to simulate the network
// problem between TiDB and TiKV.
func WithHijackClient(wrap func(Client) Client) MockTiKVStoreOption {
	return func(c *mockOptions) {
		c.clientHijack = wrap
	}
}

// WithHijackPDClient hijacks PD client's behavior, makes it easy to simulate the network
// problem between TiDB and PD, such as GetTS too slow, GetStore or GetRegion fail.
func WithHijackPDClient(wrap func(pd.Client) pd.Client) MockTiKVStoreOption {
	return func(c *mockOptions) {
		c.pdClientHijack = wrap
	}
}

// WithCluster provides the customized cluster.
func WithCluster(cluster *mocktikv.Cluster) MockTiKVStoreOption {
	return func(c *mockOptions) {
		c.cluster = cluster
	}
}

// WithMVCCStore provides the customized mvcc store.
func WithMVCCStore(store mocktikv.MVCCStore) MockTiKVStoreOption {
	return func(c *mockOptions) {
		c.mvccStore = store
	}
}

// WithPath specifies the mocktikv path.
func WithPath(path string) MockTiKVStoreOption {
	return func(c *mockOptions) {
		c.path = path
	}
}

// NewMockTikvStore creates a mocked tikv store, the path is the file path to store the data.
// If path is an empty string, a memory storage will be created.
func NewMockTikvStore(options ...MockTiKVStoreOption) (kv.Storage, error) {
	var opt mockOptions
	for _, f := range options {
		f(&opt)
	}

	cluster := opt.cluster
	if cluster == nil {
		cluster = mocktikv.NewCluster()
		mocktikv.BootstrapWithSingleStore(cluster)
	}

	mvccStore := opt.mvccStore
	if mvccStore == nil {
		mvccStore = mocktikv.NewMvccStore()
	}

	client := Client(mocktikv.NewRPCClient(cluster, mvccStore))
	if opt.clientHijack != nil {
		client = opt.clientHijack(client)
	}

	// Make sure the uuid is unique.
	partID := fmt.Sprintf("%05d", rand.Intn(100000))
	uuid := fmt.Sprintf("mock-tikv-store-%v-%v", time.Now().Unix(), partID)
	pdCli := pd.Client(&codecPDClient{mocktikv.NewPDClient(cluster)})
	if opt.pdClientHijack != nil {
		pdCli = opt.pdClientHijack(pdCli)
	}

	return newTikvStore(uuid, pdCli, client, false)
}

func (s *tikvStore) Begin() (kv.Transaction, error) {
	txn, err := newTiKVTxn(s)
	if err != nil {
		return nil, errors.Trace(err)
	}
	txnCounter.Inc()
	return txn, nil
}

// BeginWithStartTS begins a transaction with startTS.
func (s *tikvStore) BeginWithStartTS(startTS uint64) (kv.Transaction, error) {
	txn, err := newTikvTxnWithStartTS(s, startTS)
	if err != nil {
		return nil, errors.Trace(err)
	}
	txnCounter.Inc()
	return txn, nil
}

func (s *tikvStore) GetSnapshot(ver kv.Version) (kv.Snapshot, error) {
	snapshot := newTiKVSnapshot(s, ver)
	snapshotCounter.Inc()
	return snapshot, nil
}

func (s *tikvStore) Close() error {
	mc.Lock()
	defer mc.Unlock()

	delete(mc.cache, s.uuid)
	s.oracle.Close()
	s.pdClient.Close()
	if s.gcWorker != nil {
		s.gcWorker.Close()
	}

	close(s.spMsg)

	if err := s.client.Close(); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (s *tikvStore) UUID() string {
	return s.uuid
}

func (s *tikvStore) CurrentVersion() (kv.Version, error) {
	bo := NewBackoffer(tsoMaxBackoff, goctx.Background())
	startTS, err := s.getTimestampWithRetry(bo)
	if err != nil {
		return kv.NewVersion(0), errors.Trace(err)
	}
	return kv.NewVersion(startTS), nil
}

func (s *tikvStore) getTimestampWithRetry(bo *Backoffer) (uint64, error) {
	for {
		startTS, err := s.oracle.GetTimestamp(bo.ctx)
		if err == nil {
			return startTS, nil
		}
		err = bo.Backoff(boPDRPC, errors.Errorf("get timestamp failed: %v", err))
		if err != nil {
			return 0, errors.Trace(err)
		}
	}
}

func (s *tikvStore) GetClient() kv.Client {
	txnCmdCounter.WithLabelValues("get_client").Inc()
	return &CopClient{
		store: s,
	}
}

func (s *tikvStore) GetOracle() oracle.Oracle {
	return s.oracle
}

func (s *tikvStore) SupportDeleteRange() (supported bool) {
	if s.mock {
		return false
	}
	return true
}

func (s *tikvStore) SendReq(bo *Backoffer, req *tikvrpc.Request, regionID RegionVerID, timeout time.Duration) (*tikvrpc.Response, error) {
	sender := NewRegionRequestSender(s.regionCache, s.client, kvrpcpb.IsolationLevel_SI)
	return sender.SendReq(bo, req, regionID, timeout)
}

func (s *tikvStore) GetRegionCache() *RegionCache {
	return s.regionCache
}

// ParseEtcdAddr parses path to etcd address list
func ParseEtcdAddr(path string) (etcdAddrs []string, err error) {
	etcdAddrs, _, err = parsePath(path)
	return
}

func parsePath(path string) (etcdAddrs []string, disableGC bool, err error) {
	var u *url.URL
	u, err = url.Parse(path)
	if err != nil {
		err = errors.Trace(err)
		return
	}
	if strings.ToLower(u.Scheme) != "tikv" {
		err = errors.Errorf("Uri scheme expected[tikv] but found [%s]", u.Scheme)
		log.Error(err)
		return
	}
	switch strings.ToLower(u.Query().Get("disableGC")) {
	case "true":
		disableGC = true
	case "false", "":
	default:
		err = errors.New("disableGC flag should be true/false")
		return
	}
	etcdAddrs = strings.Split(u.Host, ",")
	return
}

func init() {
	mc.cache = make(map[string]*tikvStore)
	rand.Seed(time.Now().UnixNano())
}
