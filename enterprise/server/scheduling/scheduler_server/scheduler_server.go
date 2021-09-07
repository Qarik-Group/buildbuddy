package scheduler_server

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/scheduling/executor_handle"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/tasksize"
	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/resources"
	"github.com/buildbuddy-io/buildbuddy/server/util/background"
	"github.com/buildbuddy-io/buildbuddy/server/util/grpc_client"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/perms"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/buildbuddy-io/buildbuddy/server/util/timeutil"
	"github.com/go-redis/redis/v8"
	"github.com/golang/protobuf/proto"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"

	akpb "github.com/buildbuddy-io/buildbuddy/proto/api_key"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	scpb "github.com/buildbuddy-io/buildbuddy/proto/scheduler"
)

const (
	leaseInterval    = 10 * time.Second
	leaseGracePeriod = 10 * time.Second

	// This number controls how many reservations the scheduler will
	// enqueue (across executor nodes) for each task. Typically this
	// is 2 -- we've increased it to 3 because each executor will
	// work on tasks "as they fit" into available RAM, and this gives
	// us a slightly higher chance of a task being completed at the
	// cost of slightly more network traffic / queueing latency.
	probesPerTask = 3

	// How stale to allow the set of execution backends to be. If a new
	// request to enqueue work is recieved and the set of execution nodes
	// was fetched more than this duration ago, they will be re-fetched.
	maxAllowedExecutionNodesStaleness = 10 * time.Second

	// The maximum number of times a task may be re-enqueued.
	maxTaskAttemptCount = 5

	// Number of unclaimed tasks to try to assign to a node that newly joined.
	tasksToEnqueueOnJoin = 20

	// Maximum task TTL in Redis.
	taskTTL = 24 * time.Hour

	// Names of task fields in Redis task hash.
	redisTaskProtoField       = "taskProto"
	redisTaskMetadataField    = "schedulingMetadataProto"
	redisTaskQueuedAtUsec     = "queuedAtUsec"
	redisTaskAttempCountField = "attemptCount"
	redisTaskClaimedField     = "claimed"

	// Maximum number of unclaimed task IDs we track per pool.
	maxUnclaimedTasksTracked = 10_000
	// TTL for sets used to track unclaimed tasks in Redis. TTL is extended when new tasks are added.
	unclaimedTaskSetTTL = 1 * time.Hour

	unusedSchedulerClientExpiration    = 5 * time.Minute
	unusedSchedulerClientCheckInterval = 1 * time.Minute

	// Timeout when making EnqueueTaskReservation RPC to a different scheduler.
	schedulerEnqueueTaskReservationTimeout      = 3 * time.Second
	schedulerEnqueueTaskReservationFailureSleep = 1 * time.Second

	removeExecutorCleanupTimeout = 15 * time.Second

	// How often we revalidate credentials for an open registration stream.
	checkRegistrationCredentialsInterval = 5 * time.Minute
)

var (
	queueWaitTimeMs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "queue_wait_time_ms",
		Help:    "WorkQueue wait time [milliseconds]",
		Buckets: prometheus.ExponentialBuckets(1, 2, 20),
	})
	// Claim field is set only if task exists & claim field is not present.
	redisAcquireClaim = redis.NewScript(`
		if redis.call("exists", KEYS[1]) == 1 and redis.call("hexists", KEYS[1], "claimed") == 0 then 
			return redis.call("hset", KEYS[1], "claimed", "1") 
		else 
			return 0 
		end`)
	// Claim field is removed only if it's present.
	redisReleaseClaim = redis.NewScript(`
		if redis.call("hget", KEYS[1], "claimed") == "1" then 
			return redis.call("hdel", KEYS[1], "claimed")
		else 
			return 0 
		end`)
	// Task deleted if claim field is present.
	redisDeleteClaimedTask = redis.NewScript(`
		if redis.call("hget", KEYS[1], "claimed") == "1" then 
			return redis.call("del", KEYS[1]) 
		else 
			return 0 
		end`)
)

func init() {
	prometheus.MustRegister(queueWaitTimeMs)
}

type executionNode struct {
	executorID            string
	assignableMemoryBytes int64
	assignableMilliCpu    int64
	// Optional host:port of the scheduler to which the executor is connected. Only set for executors connecting using
	// the "task streaming" API.
	schedulerHostPort string
	// Optional handle for locally connected executor that can be used to enqueue task reservations.
	handle executor_handle.ExecutorHandle
}

func (en *executionNode) GetSchedulerURI() string {
	if en.schedulerHostPort == "" {
		return ""
	}
	return "grpc://" + en.schedulerHostPort
}

func (en *executionNode) String() string {
	if en.handle != nil {
		return fmt.Sprintf("connected executor(%s)", en.executorID)
	}
	return fmt.Sprintf("executor(%s) @ scheduler(%s)", en.executorID, en.schedulerHostPort)
}

func (en *executionNode) GetExecutorID() string {
	return en.executorID
}

// TODO(bduffany): Expand interfaces.ExecutionNode interface to include all
// executionNode functionality, then use interfaces.ExecutionNode everywhere.

func toNodeInterfaces(nodes []*executionNode) []interfaces.ExecutionNode {
	out := make([]interfaces.ExecutionNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node)
	}
	return out
}

func fromNodeInterfaces(nodes []interfaces.ExecutionNode) ([]*executionNode, error) {
	out := make([]*executionNode, 0, len(nodes))
	for _, node := range nodes {
		en, ok := node.(*executionNode)
		if !ok {
			return nil, status.InternalError("failed to convert executionNode to interface; this should never happen")
		}
		out = append(out, en)
	}
	return out, nil
}

type nodePoolKey struct {
	groupID string
	os      string
	arch    string
	pool    string
}

func (k *nodePoolKey) redisKeySuffix() string {
	key := ""
	if k.groupID != "" {
		key += k.groupID + "-"
	}
	return key + fmt.Sprintf("%s-%s-%s", k.os, k.arch, k.pool)
}

func (k *nodePoolKey) redisPoolKey() string {
	return "executorPool/" + k.redisKeySuffix()
}

func (k *nodePoolKey) redisUnclaimedTasksKey() string {
	return "unclaimedTasks/" + k.redisKeySuffix()
}

type nodePool struct {
	env       environment.Env
	rdb       *redis.Client
	mu        sync.Mutex
	lastFetch time.Time
	nodes     []*executionNode
	key       nodePoolKey
	// Executors that are currently connected to this instance of the scheduler server.
	connectedExecutors []*executionNode
}

func newNodePool(env environment.Env, key nodePoolKey) *nodePool {
	np := &nodePool{
		env: env,
		key: key,
		rdb: env.GetRemoteExecutionRedisClient(),
	}
	return np
}

func (np *nodePool) fetchExecutionNodes(ctx context.Context) ([]*executionNode, error) {
	redisExecutors, err := np.rdb.HGetAll(ctx, np.key.redisPoolKey()).Result()
	if err != nil {
		return nil, err
	}
	var executors []*executionNode
	for id, data := range redisExecutors {
		node := &scpb.RegisteredExecutionNode{}
		err := proto.Unmarshal([]byte(data), node)
		if err != nil {
			return nil, err
		}
		executors = append(executors, &executionNode{
			executorID:            id,
			schedulerHostPort:     node.GetSchedulerHostPort(),
			assignableMemoryBytes: node.GetRegistration().GetAssignableMemoryBytes(),
			assignableMilliCpu:    node.GetRegistration().GetAssignableMilliCpu(),
		})
	}

	return executors, nil
}

func (np *nodePool) RefreshNodes(ctx context.Context) error {
	np.mu.Lock()
	defer np.mu.Unlock()
	if np.lastFetch.Unix() > time.Now().Add(-1*maxAllowedExecutionNodesStaleness).Unix() && len(np.nodes) > 0 {
		return nil
	}
	nodes, err := np.fetchExecutionNodes(ctx)
	if err != nil {
		return err
	}
	np.nodes = nodes
	np.lastFetch = time.Now()
	return nil
}

func (np *nodePool) NodeCount(ctx context.Context, taskSize *scpb.TaskSize) (int, error) {
	if err := np.RefreshNodes(ctx); err != nil {
		return 0, err
	}
	if len(np.nodes) == 0 {
		return 0, status.UnavailableErrorf("No registered executors in pool %q with os %q with arch %q.", np.key.pool, np.key.os, np.key.arch)
	}

	fitCount := 0
	for _, node := range np.nodes {
		if int64(float64(node.assignableMemoryBytes)*tasksize.MaxResourceCapacityRatio) >= taskSize.GetEstimatedMemoryBytes() &&
			int64(float64(node.assignableMilliCpu)*tasksize.MaxResourceCapacityRatio) >= taskSize.GetEstimatedMilliCpu() {
			fitCount++
		}
	}

	if fitCount == 0 {
		return 0, status.UnavailableErrorf("No registered executors in pool %q with os %q with arch %q can fit a task with %d milli-cpu and %d bytes of memory.", np.key.pool, np.key.os, np.key.arch, taskSize.GetEstimatedMilliCpu(), taskSize.GetEstimatedMemoryBytes())
	}

	return fitCount, nil
}

func (np *nodePool) AddConnectedExecutor(id string, mem int64, cpu int64, handle executor_handle.ExecutorHandle) bool {
	np.mu.Lock()
	defer np.mu.Unlock()
	for _, e := range np.connectedExecutors {
		if e.executorID == id {
			return false
		}
	}
	np.connectedExecutors = append(np.connectedExecutors, &executionNode{
		executorID:            id,
		handle:                handle,
		assignableMemoryBytes: mem,
		assignableMilliCpu:    cpu,
	})
	return true
}

func (np *nodePool) RemoveConnectedExecutor(id string, handle executor_handle.ExecutorHandle) bool {
	np.mu.Lock()
	defer np.mu.Unlock()
	for i, e := range np.connectedExecutors {
		if e.executorID == id {
			np.connectedExecutors[i] = np.connectedExecutors[len(np.connectedExecutors)-1]
			np.connectedExecutors = np.connectedExecutors[:len(np.connectedExecutors)-1]
			return true
		}
	}
	return false
}

func (np *nodePool) FindConnectedExecutorByID(executorID string) *executionNode {
	if executorID == "" {
		return nil
	}
	np.mu.Lock()
	defer np.mu.Unlock()
	for _, node := range np.connectedExecutors {
		if node.GetExecutorID() == executorID {
			return node
		}
	}
	return nil
}

func (np *nodePool) AddUnclaimedTask(ctx context.Context, taskID string) error {
	key := np.key.redisUnclaimedTasksKey()
	m := &redis.Z{
		Member: taskID,
		Score:  float64(time.Now().Unix()),
	}
	err := np.rdb.ZAdd(ctx, key, m).Err()
	if err != nil {
		return err
	}
	err = np.rdb.Expire(ctx, key, unclaimedTaskSetTTL).Err()
	if err != nil {
		return err
	}

	// Trim the set if necessary.
	// The next 2 commands are not atomic but it's okay if the list length is not exactly what we want.
	n, err := np.rdb.ZCard(ctx, key).Result()
	if err != nil {
		return err
	}
	if n > maxUnclaimedTasksTracked {
		// Trim the oldest tasks. We use the task insertion timestamp as the score so the oldest task is at rank 0, next
		// oldest is at rank 1 and so on. We subtract 1 because the indexes are inclusive.
		return np.rdb.ZRemRangeByRank(ctx, key, 0, n-maxUnclaimedTasksTracked-1).Err()
	}
	return nil
}

func (np *nodePool) RemoveUnclaimedTask(ctx context.Context, taskID string) error {
	return np.rdb.ZRem(ctx, np.key.redisUnclaimedTasksKey(), taskID).Err()
}

func (np *nodePool) SampleUnclaimedTasks(ctx context.Context, n int) ([]string, error) {
	// Get all task IDs. Redis >=6.2 has a ZRANDMEMBER command, but we don't want to assume that onprem customers have a
	// relatively new Redis version.
	unclaimed, err := np.rdb.ZRange(ctx, np.key.redisUnclaimedTasksKey(), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	// Random sample (without replacement) up to `count` tasks from the
	// returned results.
	rand.Shuffle(len(unclaimed), func(i, j int) {
		unclaimed[i], unclaimed[j] = unclaimed[j], unclaimed[i]
	})
	return unclaimed[:minInt(n, len(unclaimed))], nil
}

type persistedTask struct {
	taskID          string
	metadata        *scpb.SchedulingMetadata
	serializedTask  []byte
	queuedTimestamp time.Time
	attemptCount    int64
}

type schedulerClient struct {
	scpb.SchedulerClient
	conn       *grpc.ClientConn
	lastAccess time.Time
}

type schedulerClientCache struct {
	mu      sync.Mutex
	clients map[string]schedulerClient
}

func newSchedulerClientCache() *schedulerClientCache {
	cache := &schedulerClientCache{clients: make(map[string]schedulerClient)}
	cache.startExpirer()
	return cache
}

func (c *schedulerClientCache) startExpirer() {
	go func() {
		for {
			c.mu.Lock()
			for addr, client := range c.clients {
				if time.Now().Sub(client.lastAccess) > unusedSchedulerClientExpiration {
					log.Infof("Expiring idle scheduler client for %q", addr)
					client.conn.Close()
					delete(c.clients, addr)
				}
			}
			c.mu.Unlock()
			time.Sleep(unusedSchedulerClientCheckInterval)
		}
	}()
}

func (c *schedulerClientCache) get(schedulerAddr string) (schedulerClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	client, ok := c.clients[schedulerAddr]
	if !ok {
		log.Infof("Creating new scheduler client for %q", schedulerAddr)
		// This is non-blocking so it's OK to hold the lock.
		conn, err := grpc_client.DialTarget(schedulerAddr)
		if err != nil {
			return schedulerClient{}, status.UnavailableErrorf("could not dial scheduler: %s", err)
		}
		client = schedulerClient{SchedulerClient: scpb.NewSchedulerClient(conn), conn: conn}
		c.clients[schedulerAddr] = client
	}
	client.lastAccess = time.Now()
	return client, nil
}

// Options for overriding server behavior needed for testing.
type Options struct {
	LocalPortOverride            int32
	RequireExecutorAuthorization bool
}

type SchedulerServer struct {
	env                  environment.Env
	rdb                  *redis.Client
	taskRouter           interfaces.TaskRouter
	schedulerClientCache *schedulerClientCache
	shuttingDown         <-chan struct{}
	// host:port at which this scheduler can be reached
	ownHostPort string

	// If enabled, users may register their own executors.
	// When enabled, the executor group ID becomes part of the executor key.
	enableUserOwnedExecutors bool
	// If enabled, executors will be required to present an API key with appropriate capabilities in order to register.
	requireExecutorAuthorization bool

	mu    sync.RWMutex
	pools map[nodePoolKey]*nodePool
}

func NewSchedulerServer(env environment.Env) (*SchedulerServer, error) {
	return NewSchedulerServerWithOptions(env, &Options{})
}

func NewSchedulerServerWithOptions(env environment.Env, options *Options) (*SchedulerServer, error) {
	if env.GetRemoteExecutionRedisClient() == nil {
		return nil, status.FailedPreconditionErrorf("Redis is required for remote execution")
	}

	enableUserOwnedExecutors := false
	requireExecutorAuthorization := false
	if conf := env.GetConfigurator().GetRemoteExecutionConfig(); conf != nil {
		enableUserOwnedExecutors = conf.EnableUserOwnedExecutors
		requireExecutorAuthorization = conf.RequireExecutorAuthorization
	}

	if options.RequireExecutorAuthorization {
		requireExecutorAuthorization = true
	}

	shuttingDown := make(chan struct{})
	env.GetHealthChecker().RegisterShutdownFunction(func(ctx context.Context) error {
		close(shuttingDown)
		return nil
	})

	taskRouter := env.GetTaskRouter()
	if taskRouter == nil {
		return nil, status.FailedPreconditionError("Missing task router in env")
	}

	ownHostname, err := resources.GetMyHostname()
	if err != nil {
		return nil, status.UnknownErrorf("Could not determine own hostname: %s", err)
	}
	ownPort := options.LocalPortOverride
	if ownPort == 0 {
		ownPort, err = resources.GetMyPort()
		if err != nil {
			return nil, status.UnknownErrorf("Could not determine own port: %s", err)
		}
	}

	s := &SchedulerServer{
		env:                          env,
		pools:                        make(map[nodePoolKey]*nodePool),
		rdb:                          env.GetRemoteExecutionRedisClient(),
		taskRouter:                   taskRouter,
		schedulerClientCache:         newSchedulerClientCache(),
		shuttingDown:                 shuttingDown,
		enableUserOwnedExecutors:     enableUserOwnedExecutors,
		requireExecutorAuthorization: requireExecutorAuthorization,
		ownHostPort:                  fmt.Sprintf("%s:%d", ownHostname, ownPort),
	}
	return s, nil
}

func (s *SchedulerServer) GetGroupIDAndDefaultPoolForUser(ctx context.Context) (string, string, error) {
	defaultPool := s.env.GetConfigurator().GetRemoteExecutionConfig().DefaultPoolName
	if !s.enableUserOwnedExecutors {
		return "", defaultPool, nil
	}

	user, err := perms.AuthenticatedUser(ctx, s.env)
	if err != nil {
		if s.env.GetConfigurator().GetAnonymousUsageEnabled() {
			return s.env.GetConfigurator().GetRemoteExecutionConfig().SharedExecutorPoolGroupID, defaultPool, nil
		}
		return "", "", err
	}
	if user.GetUseGroupOwnedExecutors() {
		return user.GetGroupID(), "", nil
	}
	return s.env.GetConfigurator().GetRemoteExecutionConfig().SharedExecutorPoolGroupID, defaultPool, nil
}

func (s *SchedulerServer) checkPreconditions(node *scpb.ExecutionNode) error {
	if node.GetHost() == "" || node.GetPort() == 0 {
		return status.FailedPreconditionErrorf("Cannot register node with empty host/port: %s:%d", node.GetHost(), node.GetPort())
	}

	return nil
}

func (s *SchedulerServer) RemoveConnectedExecutor(ctx context.Context, handle executor_handle.ExecutorHandle, node *scpb.ExecutionNode) {
	nodePoolKey := nodePoolKey{os: node.GetOs(), arch: node.GetArch(), pool: node.GetPool()}
	if s.enableUserOwnedExecutors {
		nodePoolKey.groupID = handle.GroupID()
	}
	pool, ok := s.getPool(nodePoolKey)
	if ok {
		if !pool.RemoveConnectedExecutor(node.GetExecutorId(), handle) {
			log.Warningf("Executor %q not in pool %+v", node.GetExecutorId(), nodePoolKey)
		}
	} else {
		log.Warningf("Tried to remove executor %q for unknown pool %+v", node.GetExecutorId(), nodePoolKey)
	}

	// Don't use the stream context since we want to do cleanup when stream context is cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), removeExecutorCleanupTimeout)
	defer cancel()
	addr := fmt.Sprintf("%s:%d", node.GetHost(), node.GetPort())
	if err := s.deleteNode(ctx, node, nodePoolKey); err != nil {
		log.Warningf("Scheduler: could not unregister node %q: %s", addr, err)
		return
	}
	log.Infof("Scheduler: unregistered worker node: %q", addr)
}

func (s *SchedulerServer) deleteNode(ctx context.Context, node *scpb.ExecutionNode, poolKey nodePoolKey) error {
	if err := s.checkPreconditions(node); err != nil {
		return err
	}
	return s.rdb.HDel(ctx, poolKey.redisPoolKey(), node.GetExecutorId()).Err()
}

func (s *SchedulerServer) AddConnectedExecutor(ctx context.Context, handle executor_handle.ExecutorHandle, node *scpb.ExecutionNode) error {
	poolKey := nodePoolKey{os: node.GetOs(), arch: node.GetArch(), pool: node.GetPool()}
	if s.enableUserOwnedExecutors {
		poolKey.groupID = handle.GroupID()
	}

	err := s.insertOrUpdateNode(ctx, handle, node, poolKey)
	if err != nil {
		return err
	}

	pool := s.getOrCreatePool(poolKey)
	newExecutor := pool.AddConnectedExecutor(node.GetExecutorId(), node.GetAssignableMemoryBytes(), node.GetAssignableMilliCpu(), handle)
	if !newExecutor {
		return nil
	}
	addr := fmt.Sprintf("%s:%d", node.GetHost(), node.GetPort())
	log.Infof("Scheduler: registered worker node: %q %+v", addr, poolKey)

	go func() {
		if err := s.assignWorkToNode(ctx, handle, poolKey); err != nil {
			log.Warningf("Failed to assign work to new node: %s", err.Error())
		}
	}()
	return nil

}

func (s *SchedulerServer) redisKeyForExecutorPools(groupID string) string {
	key := "executorPools/"
	if s.enableUserOwnedExecutors {
		key += groupID
	}
	return key
}

func (s *SchedulerServer) insertOrUpdateNode(ctx context.Context, executorHandle executor_handle.ExecutorHandle, node *scpb.ExecutionNode, poolKey nodePoolKey) error {
	if err := s.checkPreconditions(node); err != nil {
		return err
	}

	permissions := 0
	if s.requireExecutorAuthorization {
		permissions = perms.GROUP_WRITE | perms.GROUP_READ
	} else {
		permissions = perms.OTHERS_READ
	}
	acl := perms.ToACLProto(nil /* userID= */, executorHandle.GroupID(), permissions)

	groupID := executorHandle.GroupID()

	r := &scpb.RegisteredExecutionNode{
		Registration:      node,
		SchedulerHostPort: s.ownHostPort,
		GroupId:           groupID,
		Acl:               acl,
	}
	b, err := proto.Marshal(r)
	if err != nil {
		return err
	}

	pipe := s.rdb.TxPipeline()
	poolRedisKey := poolKey.redisPoolKey()
	pipe.HSet(ctx, poolRedisKey, node.GetExecutorId(), b)
	pipe.SAdd(ctx, s.redisKeyForExecutorPools(groupID), poolRedisKey)
	_, err = pipe.Exec(ctx)
	return err
}

func (s *SchedulerServer) processExecutorStream(ctx context.Context, handle executor_handle.ExecutorHandle) error {
	var registeredNode *scpb.ExecutionNode
	defer func() {
		if registeredNode == nil {
			return
		}
		s.RemoveConnectedExecutor(ctx, handle, registeredNode)
	}()

	registrationChan := make(chan *scpb.ExecutionNode, 1)
	errChan := make(chan error)
	go func() {
		for {
			registration, err := handle.RecvRegistration()
			if err == io.EOF {
				close(registrationChan)
				break
			}
			if err != nil {
				errChan <- err
				break
			}
			registrationChan <- registration
		}
	}()

	checkCredentialsTicker := time.NewTicker(checkRegistrationCredentialsInterval)

	for {
		select {
		case <-s.shuttingDown:
			return status.CanceledError("server is shutting down")
		case err := <-errChan:
			return err
		case registration, ok := <-registrationChan:
			if !ok {
				return nil
			}
			if err := s.AddConnectedExecutor(ctx, handle, registration); err != nil {
				return err
			}
			registeredNode = registration
		case <-checkCredentialsTicker.C:
			if _, err := s.authorizeExecutor(ctx); err != nil {
				if status.IsPermissionDeniedError(err) || status.IsUnauthenticatedError(err) {
					return err
				}
				log.Warningf("could not revalidate executor registration: %s", err)
			}
		}
	}
}

func (s *SchedulerServer) authorizeExecutor(ctx context.Context) (string, error) {
	if !s.requireExecutorAuthorization {
		return "", nil
	}

	auth := s.env.GetAuthenticator()
	if auth == nil {
		return "", status.FailedPreconditionError("executor authorization required, but authenticator is not set")
	}
	// We intentionally use AuthenticateGRPCRequest instead of AuthenticatedUser to ensure that we refresh the
	// credentials to handle the case where the API key is deleted (or capabilities are updated) after the stream was
	// created.
	user, err := auth.AuthenticateGRPCRequest(ctx)
	if err != nil {
		return "", err
	}
	if !user.HasCapability(akpb.ApiKey_REGISTER_EXECUTOR_CAPABILITY) {
		return "", status.PermissionDeniedError("API key is missing executor registration capability")
	}
	return user.GetGroupID(), nil
}

func (s *SchedulerServer) RegisterAndStreamWork(stream scpb.Scheduler_RegisterAndStreamWorkServer) error {
	groupID, err := s.authorizeExecutor(stream.Context())
	if err != nil {
		return err
	}

	handle := executor_handle.NewRegistrationAndTasksExecutorHandle(stream, groupID)
	return s.processExecutorStream(stream.Context(), handle)
}

func (s *SchedulerServer) assignWorkToNode(ctx context.Context, handle executor_handle.ExecutorHandle, nodePoolKey nodePoolKey) error {
	tasks, err := s.sampleUnclaimedTasks(ctx, tasksToEnqueueOnJoin, nodePoolKey)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}

	var reqs []*scpb.EnqueueTaskReservationRequest
	for _, task := range tasks {
		req := &scpb.EnqueueTaskReservationRequest{
			TaskId:   task.taskID,
			TaskSize: task.metadata.GetTaskSize(),
		}
		reqs = append(reqs, req)
	}

	for _, req := range reqs {
		if _, err = handle.EnqueueTaskReservation(ctx, req); err != nil {
			return err
		}
	}
	return nil
}

func redisKeyForTask(taskID string) string {
	return "task/" + taskID
}

func (s *SchedulerServer) insertTask(ctx context.Context, taskID string, metadata *scpb.SchedulingMetadata, serializedTask []byte) error {
	serializedMetadata, err := proto.Marshal(metadata)
	if err != nil {
		return status.InternalErrorf("unable to serialize scheduling metadata: %v", err)
	}

	props := map[string]interface{}{
		redisTaskProtoField:       serializedTask,
		redisTaskMetadataField:    serializedMetadata,
		redisTaskQueuedAtUsec:     timeutil.ToUsec(time.Now()),
		redisTaskAttempCountField: 0,
	}
	c, err := s.rdb.HSet(ctx, redisKeyForTask(taskID), props).Result()
	if err != nil {
		return err
	}
	if c == 0 {
		return status.AlreadyExistsErrorf("task %s already exists", taskID)
	}
	ok, err := s.rdb.Expire(ctx, redisKeyForTask(taskID), taskTTL).Result()
	if err != nil {
		return err
	}
	if !ok {
		return status.DataLossErrorf("task %s disappeared before we could set TTL", taskID)
	}
	return nil
}

func (s *SchedulerServer) deleteClaimedTask(ctx context.Context, taskID string) error {
	// The script will return 1 if the task is claimed & has been deleted.
	r, err := redisDeleteClaimedTask.Run(ctx, s.rdb, []string{redisKeyForTask(taskID)}).Result()
	if err != nil {
		return err
	}
	if c, ok := r.(int64); !ok || c != 1 {
		return status.NotFoundErrorf("unable to delete claimed task %s", taskID)
	}
	return nil
}

func (s *SchedulerServer) unclaimTask(ctx context.Context, taskID string) error {
	// The script will return 1 if the task is claimed & claim has been released.
	r, err := redisReleaseClaim.Run(ctx, s.rdb, []string{redisKeyForTask(taskID)}).Result()
	if err != nil {
		return err
	}
	if c, ok := r.(int64); !ok || c != 1 {
		return status.NotFoundErrorf("unable to release task claim for task %s", taskID)
	}
	return nil
}

func (s *SchedulerServer) claimTask(ctx context.Context, taskID string, claimTime time.Time) error {
	r, err := redisAcquireClaim.Run(ctx, s.rdb, []string{redisKeyForTask(taskID)}).Result()
	if err != nil {
		return err
	}

	// Someone else claimed the task.
	if c, ok := r.(int64); !ok || c != 1 {
		return status.NotFoundErrorf("unable to claim task: %q", taskID)
	}

	err = s.rdb.HIncrBy(ctx, redisKeyForTask(taskID), redisTaskAttempCountField, 1).Err()
	if err != nil {
		return err
	}

	return nil
}

func (s *SchedulerServer) readTasks(ctx context.Context, taskIDs []string) ([]*persistedTask, error) {
	var tasks []*persistedTask

	for _, taskID := range taskIDs {
		task, err := s.readTask(ctx, taskID)
		if err != nil {
			if !status.IsNotFoundError(err) {
				log.Errorf("error reading task from redis: %v", err)
			}
			continue
		}
		tasks = append(tasks, task)
	}

	return tasks, nil
}

func (s *SchedulerServer) getPool(key nodePoolKey) (*nodePool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	nodePool, ok := s.pools[key]
	return nodePool, ok
}

func (s *SchedulerServer) getOrCreatePool(key nodePoolKey) *nodePool {
	s.mu.RLock()
	nodePool, ok := s.pools[key]
	s.mu.RUnlock()
	if ok {
		return nodePool
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	nodePool, ok = s.pools[key]
	if ok {
		return nodePool
	}
	nodePool = newNodePool(s.env, key)
	s.pools[key] = nodePool
	return nodePool
}

func (s *SchedulerServer) sampleUnclaimedTasks(ctx context.Context, count int, nodePoolKey nodePoolKey) ([]*persistedTask, error) {
	nodePool, ok := s.getPool(nodePoolKey)
	if !ok {
		return nil, nil
	}
	taskIDs, err := nodePool.SampleUnclaimedTasks(ctx, count)
	if err != nil {
		return nil, err
	}
	tasks, err := s.readTasks(ctx, taskIDs)
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

func (s *SchedulerServer) readTask(ctx context.Context, taskID string) (*persistedTask, error) {
	if s.rdb == nil {
		return nil, status.FailedPreconditionError("redis client not set")
	}

	fields := []string{
		redisTaskProtoField,
		redisTaskMetadataField,
		redisTaskQueuedAtUsec,
		redisTaskAttempCountField,
	}
	vals, err := s.rdb.HMGet(ctx, redisKeyForTask(taskID), fields...).Result()
	if err != nil {
		return nil, status.InternalErrorf("could not read task from redis: %v", err)
	}
	if len(vals) != len(fields) {
		return nil, status.FailedPreconditionErrorf("unexpected # of returned values in redis response: %+v", vals)
	}
	if vals[0] == nil {
		return nil, status.NotFoundErrorf("task %q not found", taskID)
	}

	// Task field.
	taskString, ok := vals[0].(string)
	if !ok {
		return nil, status.InvalidArgumentErrorf("unexpected type %T for task", vals[0])
	}
	serializedTask := []byte(taskString)

	// Scheduling metadata field.
	metadataString, ok := vals[1].(string)
	if !ok {
		return nil, status.InvalidArgumentErrorf("unexpected type %T for metadata", vals[1])
	}
	metadata := &scpb.SchedulingMetadata{}
	err = proto.Unmarshal([]byte(metadataString), metadata)
	if err != nil {
		return nil, status.InternalErrorf("could not deserialize metadata proto: %v", err)
	}

	// Queued At Timestamp field.
	queuedAtUsecString, ok := vals[2].(string)
	if !ok {
		return nil, status.InvalidArgumentErrorf("unexpected type %T for queued at timestamp", vals[2])
	}
	queuedAtUsec, err := strconv.ParseInt(queuedAtUsecString, 10, 64)
	if err != nil {
		return nil, status.InvalidArgumentErrorf("could not parse queued at timestamp %q: %v", queuedAtUsecString, err)
	}

	// Attempt count field.
	attemptCountStr, ok := vals[3].(string)
	if !ok {
		return nil, status.InvalidArgumentErrorf("unexpected type %T for attempt count", vals[3])
	}
	attemptCount, err := strconv.ParseInt(attemptCountStr, 10, 64)
	if err != nil {
		return nil, status.InvalidArgumentErrorf("could not parse attempt count %q: %v", attemptCountStr, attemptCount)
	}

	return &persistedTask{
		taskID:          taskID,
		metadata:        metadata,
		serializedTask:  serializedTask,
		queuedTimestamp: timeutil.FromUsec(queuedAtUsec),
		attemptCount:    attemptCount,
	}, nil
}

// TODO(vadim): we should verify that the executor is authorized to read the task
func (s *SchedulerServer) LeaseTask(stream scpb.Scheduler_LeaseTaskServer) error {
	ctx := stream.Context()
	lastCheckin := time.Now()
	claimed := false
	closing := false
	taskID := ""

	executorID := "unknown"
	if p, ok := peer.FromContext(ctx); ok {
		executorID = p.Addr.String()
	}

	// If we've exited our event loop and the task is still claimed, then
	// the worker did not finish properly and we should re-enqueue it.
	defer func() {
		if !claimed {
			return
		}
		log.Warningf("LeaseTask %q exited event-loop with task still claimed. Will ReEnqueue!", taskID)
		ctx, cancel := background.ExtendContextForFinalization(ctx, 3*time.Second)
		defer cancel()
		if _, err := s.ReEnqueueTask(ctx, &scpb.ReEnqueueTaskRequest{TaskId: taskID}); err != nil {
			log.Errorf("LeaseTask %q tried to re-enqueue task but failed with err: %s", taskID, err.Error())
		} // Success case will be logged by ReEnqueueTask flow.
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			log.Debugf("LeaseTask %q got EOF", taskID)
			break
		}
		if err != nil {
			log.Debugf("LeaseTask %q recv with err: %s", taskID, err.Error())
			break
		}
		if req.GetTaskId() == "" || taskID != "" && req.GetTaskId() != taskID {
			// We don't re-enqueue in this case which is a bummer but
			// makes the code significantly simpler. Also, don't do this!
			return status.InvalidArgumentError("TaskId must be set and the same value for all requests")
		}
		taskID = req.GetTaskId()
		if time.Since(lastCheckin) > (leaseInterval + leaseGracePeriod) {
			log.Warningf("LeaseTask %q client went away after %s", taskID, time.Since(lastCheckin))
			break
		}
		rsp := &scpb.LeaseTaskResponse{
			LeaseDurationSeconds: int64(leaseInterval.Seconds()),
		}
		if !claimed {
			err = s.claimTask(ctx, taskID, time.Now())
			if err != nil {
				return err
			}
			claimed = true
			task, err := s.readTask(ctx, req.GetTaskId())
			if err != nil {
				log.Errorf("LeaseTask %q error reading task %s", taskID, err.Error())
				return err
			}

			log.Infof("LeaseTask task %q successfully claimed by executor %q", taskID, executorID)

			key := nodePoolKey{
				os:      task.metadata.GetOs(),
				arch:    task.metadata.GetArch(),
				pool:    task.metadata.GetPool(),
				groupID: task.metadata.GetExecutorGroupId(),
			}
			nodePool, ok := s.getPool(key)
			if ok {
				err := nodePool.RemoveUnclaimedTask(ctx, taskID)
				if err != nil {
					log.Warningf("Could not remove task from unclaimed list: %s", err)
				}
			}

			// Prometheus: observe queue wait time.
			ageInMillis := time.Since(task.queuedTimestamp).Milliseconds()
			queueWaitTimeMs.Observe(float64(ageInMillis))
			rsp.SerializedTask = task.serializedTask
		}

		closing = req.GetFinalize()
		if closing && claimed {
			if err := s.deleteClaimedTask(ctx, taskID); err == nil {
				claimed = false
				log.Infof("LeaseTask task %q successfully finalized by %q", taskID, executorID)
			}
		}

		rsp.ClosedCleanly = !claimed
		lastCheckin = time.Now()
		if err := stream.Send(rsp); err != nil {
			return err
		}
		if closing {
			break
		}
	}

	return nil
}

func minInt(i, j int) int {
	if i < j {
		return i
	}
	return j
}

type enqueueTaskReservationOpts struct {
	numReplicas           int
	maxAttempts           int
	alwaysScheduleLocally bool
}

func (s *SchedulerServer) enqueueTaskReservations(ctx context.Context, enqueueRequest *scpb.EnqueueTaskReservationRequest, serializedTask []byte, opts enqueueTaskReservationOpts) error {
	os := enqueueRequest.GetSchedulingMetadata().GetOs()
	arch := enqueueRequest.GetSchedulingMetadata().GetArch()
	pool := enqueueRequest.GetSchedulingMetadata().GetPool()
	groupID := enqueueRequest.GetSchedulingMetadata().GetExecutorGroupId()

	key := nodePoolKey{os: os, arch: arch, pool: pool, groupID: groupID}

	log.Infof("Enqueue task reservations for task %q with pool key %+v.", enqueueRequest.GetTaskId(), key)

	nodeBalancer := s.getOrCreatePool(key)
	nodeCount, err := nodeBalancer.NodeCount(ctx, enqueueRequest.GetTaskSize())
	if err != nil {
		return err
	}

	err = nodeBalancer.AddUnclaimedTask(ctx, enqueueRequest.GetTaskId())
	if err != nil {
		log.Warningf("Could not add task to unclaimed task list: %s", err)
	}

	probeCount := minInt(opts.numReplicas, nodeCount)
	probesSent := 0

	startTime := time.Now()
	var successfulReservations []string
	defer func() {
		log.Infof("Enqueue task reservations for task %q took %s. Reservations: [%s]",
			enqueueRequest.GetTaskId(), time.Now().Sub(startTime), strings.Join(successfulReservations, ", "))
	}()

	cmd, remoteInstanceName, err := extractRoutingProps(serializedTask)
	if err != nil {
		return err
	}

	// Note: preferredNode may be nil if the executor ID isn't specified or if
	// the executor is no longer connected.
	preferredNode := nodeBalancer.FindConnectedExecutorByID(enqueueRequest.GetExecutorId())

	attempts := 0
	var nodes []*executionNode
	sampleIndex := 0
	for probesSent < probeCount {
		attempts++
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			break
		}
		if opts.maxAttempts > 0 && attempts > opts.maxAttempts {
			return status.ResourceExhaustedErrorf("could not enqueue task reservation to executor")
		}
		if attempts > 100 {
			log.Warningf("Attempted to send probe %d times for task %q with pool key %+v. This should not happen.", attempts, enqueueRequest.GetTaskId(), key)
		}
		if sampleIndex == 0 {
			if preferredNode != nil {
				nodes = []*executionNode{preferredNode}
				// Unset the preferred node so that we fall back to random sampling
				// (in subsequent loop iterations) if the preferred node probe fails.
				preferredNode = nil
			} else {
				nodes = nodeBalancer.nodes
				if opts.alwaysScheduleLocally {
					nodes = nodeBalancer.connectedExecutors
				}
				if len(nodes) == 0 {
					return status.UnavailableErrorf("No registered executors in pool %q with os %q with arch %q.", pool, os, arch)
				}
				rankedNodes := s.taskRouter.RankNodes(ctx, cmd, remoteInstanceName, toNodeInterfaces(nodes))
				nodes, err = fromNodeInterfaces(rankedNodes)
				if err != nil {
					return err
				}
			}
		}
		if sampleIndex >= len(nodes) {
			return status.FailedPreconditionErrorf("sampleIndex %d >= %d", sampleIndex, len(nodes))
		}
		node := nodes[sampleIndex]
		sampleIndex = (sampleIndex + 1) % len(nodes)
		// Set the executor ID in case the node is owned by another scheduler, so
		// that the scheduler can prefer this node for the probe.
		enqueueRequest.ExecutorId = node.GetExecutorID()

		enqueueStart := time.Now()
		if opts.alwaysScheduleLocally {
			if node.handle == nil {
				log.Errorf("nil handle for a local executor %q", node.GetExecutorID())
				continue
			}
			_, err := node.handle.EnqueueTaskReservation(ctx, enqueueRequest)
			if err != nil {
				continue
			}
		} else {
			if node.GetSchedulerURI() == "" {
				log.Errorf("node %q has no scheduler URI", node.GetExecutorID())
				continue
			}

			schedulerClient, err := s.schedulerClientCache.get(node.GetSchedulerURI())
			if err != nil {
				log.Warningf("Could not get SchedulerClient for %q: %s", node.GetSchedulerURI(), err)
				continue
			}
			rpcCtx, cancel := context.WithTimeout(ctx, schedulerEnqueueTaskReservationTimeout)
			_, err = schedulerClient.EnqueueTaskReservation(rpcCtx, enqueueRequest)
			cancel()
			if err != nil {
				log.Warningf("EnqueueTaskReservation to %q failed: %s", node.GetSchedulerURI(), err)
				time.Sleep(schedulerEnqueueTaskReservationFailureSleep)
				continue
			}
		}
		successfulReservations = append(successfulReservations, fmt.Sprintf("%s [%s]", node.String(), time.Now().Sub(enqueueStart).String()))
		probesSent++
	}
	return nil
}

func (s *SchedulerServer) ScheduleTask(ctx context.Context, req *scpb.ScheduleTaskRequest) (*scpb.ScheduleTaskResponse, error) {
	if req.GetTaskId() == "" {
		return nil, status.FailedPreconditionError("A task_id is required")
	}
	if req.Metadata == nil {
		return nil, status.FailedPreconditionError("Scheduling metadata is required")
	}
	if req.Metadata.TaskSize == nil {
		return nil, status.FailedPreconditionError("A task_size is required")
	}
	if len(req.GetSerializedTask()) == 0 {
		return nil, status.FailedPreconditionError("Serialized task is required")
	}
	taskID := req.GetTaskId()
	metadata := req.GetMetadata()
	if err := s.insertTask(ctx, taskID, metadata, req.GetSerializedTask()); err != nil {
		return nil, err
	}
	enqueueRequest := &scpb.EnqueueTaskReservationRequest{
		TaskId:             taskID,
		TaskSize:           req.GetMetadata().GetTaskSize(),
		SchedulingMetadata: metadata,
	}

	opts := enqueueTaskReservationOpts{
		numReplicas:           probesPerTask,
		alwaysScheduleLocally: false,
	}
	if err := s.enqueueTaskReservations(ctx, enqueueRequest, req.GetSerializedTask(), opts); err != nil {
		return nil, err
	}
	return &scpb.ScheduleTaskResponse{}, nil
}

func (s *SchedulerServer) EnqueueTaskReservation(ctx context.Context, req *scpb.EnqueueTaskReservationRequest) (*scpb.EnqueueTaskReservationResponse, error) {
	// TODO(vadim): verify user is authorized to use executor pool

	opts := enqueueTaskReservationOpts{
		numReplicas:           1,
		maxAttempts:           10,
		alwaysScheduleLocally: true,
	}
	if err := s.enqueueTaskReservations(ctx, req, nil /*=serializedTask*/, opts); err != nil {
		return nil, err
	}
	return &scpb.EnqueueTaskReservationResponse{}, nil
}

func (s *SchedulerServer) ReEnqueueTask(ctx context.Context, req *scpb.ReEnqueueTaskRequest) (*scpb.ReEnqueueTaskResponse, error) {
	if true {
		log.Printf("TYLER: ReEnqueueTask was called but skipping.")
		return &scpb.ReEnqueueTaskResponse{}, nil
	}
	if req.GetTaskId() == "" {
		return nil, status.FailedPreconditionError("A task_id is required")
	}
	task, err := s.readTask(ctx, req.GetTaskId())
	if err != nil {
		return nil, err
	}
	if task.attemptCount >= maxTaskAttemptCount {
		if err := s.deleteClaimedTask(ctx, req.GetTaskId()); err != nil {
			return nil, err
		}
		return nil, status.ResourceExhaustedErrorf("Task already attempted %d times.", task.attemptCount)
	}
	_ = s.unclaimTask(ctx, req.GetTaskId()) // ignore error -- it's fine if it's already unclaimed.
	log.Debugf("ReEnqueueTask RPC for task %q", req.GetTaskId())
	enqueueRequest := &scpb.EnqueueTaskReservationRequest{
		TaskId:             req.GetTaskId(),
		TaskSize:           task.metadata.GetTaskSize(),
		SchedulingMetadata: task.metadata,
	}
	opts := enqueueTaskReservationOpts{
		numReplicas:           probesPerTask,
		alwaysScheduleLocally: false,
	}
	if err := s.enqueueTaskReservations(ctx, enqueueRequest, task.serializedTask, opts); err != nil {
		log.Errorf("ReEnqueueTask failed for task %q: %s", req.GetTaskId(), err.Error())
		return nil, err
	}
	log.Debugf("ReEnqueueTask RPC succeeded for task %q", req.GetTaskId())
	return &scpb.ReEnqueueTaskResponse{}, nil
}

func (s *SchedulerServer) getExecutionNodesFromRedis(ctx context.Context, groupID string) ([]*scpb.ExecutionNode, error) {
	user, err := perms.AuthenticatedUser(ctx, s.env)
	if err != nil {
		return nil, err
	}

	poolSetKey := s.redisKeyForExecutorPools(groupID)
	poolKeys, err := s.rdb.SMembers(ctx, poolSetKey).Result()
	if err != nil {
		return nil, err
	}
	var executionNodes []*scpb.ExecutionNode
	for _, k := range poolKeys {
		executors, err := s.rdb.HGetAll(ctx, k).Result()
		if err != nil {
			return nil, err
		}
		for _, data := range executors {
			registeredNode := &scpb.RegisteredExecutionNode{}
			if err := proto.Unmarshal([]byte(data), registeredNode); err != nil {
				return nil, err
			}

			err := perms.AuthorizeRead(&user, registeredNode.GetAcl())
			if err != nil {
				continue
			}
			executionNodes = append(executionNodes, registeredNode.GetRegistration())
		}
	}
	return executionNodes, nil
}

func (s *SchedulerServer) GetExecutionNodes(ctx context.Context, req *scpb.GetExecutionNodesRequest) (*scpb.GetExecutionNodesResponse, error) {
	groupID := req.GetRequestContext().GetGroupId()
	if groupID == "" {
		return nil, status.InvalidArgumentError("group not specified")
	}

	// If executor auth is not enabled, executors do not belong to any group.
	if !s.requireExecutorAuthorization {
		groupID = ""
	}

	executionNodes, err := s.getExecutionNodesFromRedis(ctx, groupID)
	if err != nil {
		return nil, err
	}

	userOwnedExecutorsEnabled := s.enableUserOwnedExecutors
	// Don't report user owned executors as being enabled for the shared executor group ID (i.e. the BuildBuddy group)
	if userOwnedExecutorsEnabled && groupID == s.env.GetConfigurator().GetRemoteExecutionConfig().SharedExecutorPoolGroupID {
		userOwnedExecutorsEnabled = false
	}

	return &scpb.GetExecutionNodesResponse{
		ExecutionNode:               executionNodes,
		UserOwnedExecutorsSupported: userOwnedExecutorsEnabled,
	}, nil
}

// extractRoutingProps deserializes the given task and returns the properties
// needed to route the task (command and remote instance name).
func extractRoutingProps(serializedTask []byte) (*repb.Command, string, error) {
	if serializedTask == nil {
		return nil, "", nil
	}
	task := &repb.ExecutionTask{}
	if err := proto.Unmarshal(serializedTask, task); err != nil {
		return nil, "", status.InternalErrorf("failed to unmarshal ExecutionTask: %s", err)
	}
	return task.GetCommand(), task.GetExecuteRequest().GetInstanceName(), nil
}
