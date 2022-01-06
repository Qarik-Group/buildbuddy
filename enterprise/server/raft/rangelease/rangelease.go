package rangelease

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/raft/client"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/raft/constants"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/raft/nodeliveness"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/raft/rbuilder"
	"github.com/buildbuddy-io/buildbuddy/server/util/rangemap"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/golang/protobuf/proto"

	rfpb "github.com/buildbuddy-io/buildbuddy/proto/raft"
)

const (
	// Acquire the lease for this long.
	defaultLeaseDuration = 9 * time.Second

	// Renew the lease this many seconds *before* expiry.
	defaultGracePeriod = 4 * time.Second
)

// TODO(tylerw): Maybe this belongs in keys?
func containsMetaRange(rd *rfpb.RangeDescriptor) bool {
	r := rangemap.Range{Left: rd.Left, Right: rd.Right}
	return r.Contains([]byte{constants.MinByte}) && r.Contains([]byte{constants.UnsplittableMaxByte - 1})
}

type Lease struct {
	localSender   client.LocalSender
	liveness      *nodeliveness.Liveness
	leaseDuration time.Duration
	gracePeriod   time.Duration

	rangeDescriptor *rfpb.RangeDescriptor
	mu              sync.RWMutex
	leaseRecord     *rfpb.RangeLeaseRecord

	timeUntilLeaseRenewal time.Duration
	quitLease             chan struct{}
}

func New(localSender client.LocalSender, liveness *nodeliveness.Liveness, rd *rfpb.RangeDescriptor) *Lease {
	return &Lease{
		localSender:           localSender,
		liveness:              liveness,
		leaseDuration:         defaultLeaseDuration,
		gracePeriod:           defaultGracePeriod,
		rangeDescriptor:       rd,
		mu:                    sync.RWMutex{},
		leaseRecord:           &rfpb.RangeLeaseRecord{},
		timeUntilLeaseRenewal: time.Duration(math.MaxInt64),
		quitLease:             make(chan struct{}),
	}
}

func (l *Lease) WithTimeouts(leaseDuration, gracePeriod time.Duration) *Lease {
	l.leaseDuration = leaseDuration
	l.gracePeriod = gracePeriod
	return l
}

func (l *Lease) Lease() error {
	l.quitLease = make(chan struct{})
	_, err := l.ensureValidLease(false)
	if err == nil {
		go l.keepLeaseAlive()
	}
	return err
}

func (l *Lease) Release() error {
	close(l.quitLease)
	// clear existing lease if it's valid.
	l.mu.RLock()
	valid := l.verifyLease(l.leaseRecord) == nil
	l.mu.RUnlock()

	if valid {
		l.mu.Lock()
		defer l.mu.Unlock()
		if err := l.clearLease(); err != nil {
			return err
		}
	}
	return nil
}

func (l *Lease) String() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	err := l.verifyLease(l.leaseRecord)
	if err != nil {
		return fmt.Sprintf("RangeLease(%d) invalid (%s)", l.rangeDescriptor.GetRangeId(), err)
	}
	lr := l.leaseRecord
	if nl := lr.GetNodeLiveness(); nl != nil {
		return fmt.Sprintf("RangeLease(%d) [node epoch: %d]", l.rangeDescriptor.GetRangeId(), nl.GetEpoch())
	}
	lifetime := time.Unix(0, lr.GetExpiration()).Sub(time.Now())
	return fmt.Sprintf("RangeLease(%d) [expires in: %s]", l.rangeDescriptor.GetRangeId(), lifetime)
}

func (l *Lease) verifyLease(rl *rfpb.RangeLeaseRecord) error {
	if rl == nil {
		return status.FailedPreconditionErrorf("Invalid rangeLease: nil")
	}
	if nl := rl.GetNodeLiveness(); nl != nil {
		// This is a node epoch based lease, so check node and epoch.
		return l.liveness.BlockingValidateNodeLiveness(nl)
	}

	// This is a time based lease, so check expiration time.
	expireAt := time.Unix(0, rl.GetExpiration())
	if time.Now().After(expireAt) {
		return status.FailedPreconditionErrorf("Invalid rangeLease: expired at %s", expireAt)
	}
	return nil
}

func (l *Lease) sendCasRequest(ctx context.Context, expectedValue, newVal []byte) (*rfpb.KV, error) {
	clusterID, err := l.getClusterID()
	if err != nil {
		return nil, err
	}

	leaseKey := constants.LocalRangeLeaseKey
	casRequest, err := rbuilder.NewBatchBuilder().Add(&rfpb.CASRequest{
		Kv: &rfpb.KV{
			Key:   leaseKey,
			Value: newVal,
		},
		ExpectedValue: expectedValue,
	}).ToProto()
	if err != nil {
		return nil, err
	}
	rsp, err := l.localSender.SyncProposeLocal(ctx, clusterID, casRequest)
	if err != nil {
		// This indicates a communication error proposing the message.
		return nil, err
	}
	casResponse, err := rbuilder.NewBatchResponseFromProto(rsp).CASResponse(0)
	if casResponse != nil {
		return casResponse.GetKv(), err
	}
	return nil, err
}

func (l *Lease) clearLease() error {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	var expectedValue []byte
	if l.leaseRecord != nil {
		buf, err := proto.Marshal(l.leaseRecord)
		if err != nil {
			return err
		}
		expectedValue = buf
	}

	_, err := l.sendCasRequest(ctx, expectedValue, nil)
	if err == nil {
		l.leaseRecord = nil
	}
	return err
}

func (l *Lease) assembleLeaseRequest() (*rfpb.RangeLeaseRecord, error) {
	// To prevent circular dependencies:
	//    (metarange -> range lease -> node liveness -> metarange)
	// any range that includes the metarange will be leased with a
	// time-based lease, rather than a node epoch based one.
	leaseRecord := &rfpb.RangeLeaseRecord{}
	if containsMetaRange(l.rangeDescriptor) {
		leaseRecord.Value = &rfpb.RangeLeaseRecord_Expiration{
			Expiration: time.Now().Add(l.leaseDuration).UnixNano(),
		}
	} else {
		nl, err := l.liveness.BlockingGetCurrentNodeLiveness()
		if err != nil {
			return nil, err
		}
		leaseRecord.Value = &rfpb.RangeLeaseRecord_NodeLiveness_{
			NodeLiveness: nl,
		}
	}
	return leaseRecord, nil
}

func (l *Lease) getClusterID() (uint64, error) {
	replicas := l.rangeDescriptor.GetReplicas()
	for _, replicaDescriptor := range replicas {
		return replicaDescriptor.GetClusterId(), nil
	}
	return 0, status.FailedPreconditionError("No replicas in range")
}

func (l *Lease) renewLease() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var expectedValue []byte
	if l.leaseRecord != nil {
		buf, err := proto.Marshal(l.leaseRecord)
		if err != nil {
			return err
		}
		expectedValue = buf
	}

	leaseRequest, err := l.assembleLeaseRequest()
	if err != nil {
		return err
	}
	newVal, err := proto.Marshal(leaseRequest)
	if err != nil {
		return err
	}

	if bytes.Compare(newVal, expectedValue) == 0 {
		// For node-epoch based leases, forcing renewal is kind of non-
		// sensical. Rather than prevent this at a higher level, we
		// detect the case where we are trying to set the lease to the
		// already set value, and short-circuit renewal.
		return nil
	}

	kv, err := l.sendCasRequest(ctx, expectedValue, newVal)
	if err == nil {
		// This means we set the lease succesfully.
		l.leaseRecord = leaseRequest
	} else if status.IsFailedPreconditionError(err) && strings.Contains(err.Error(), constants.CASErrorMessage) {
		// This means another lease was active -- we should save it, so that
		// we can correctly set the expected value with our next CAS request,
		// and witness its epoch so that our next set request has a higher one.
		err := proto.Unmarshal(kv.GetValue(), l.leaseRecord)
		if err != nil {
			return err
		}
	} else {
		return err
	}

	// If the lease is now set and has an expiration date in the future,
	// set time until lease renewal. This will not happen for node liveness
	// epoch based range leases.
	if l.leaseRecord != nil && l.leaseRecord.GetExpiration() != 0 {
		expiration := time.Unix(0, l.leaseRecord.GetExpiration())
		timeUntilExpiry := expiration.Sub(time.Now())
		l.timeUntilLeaseRenewal = timeUntilExpiry - l.gracePeriod
	}
	return nil
}

func (l *Lease) blockingGetValidLease() (*rfpb.RangeLeaseRecord, error) {
	l.mu.RLock()
	var rl *rfpb.RangeLeaseRecord
	if err := l.verifyLease(l.leaseRecord); err == nil {
		rl = l.leaseRecord
	}
	l.mu.RUnlock()
	if rl != nil {
		return rl, nil
	}

	rl, err := l.ensureValidLease(false /*=forceRenewal*/)
	if err != nil {
		return nil, err
	}
	return rl, nil
}

func (l *Lease) ensureValidLease(forceRenewal bool) (*rfpb.RangeLeaseRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !forceRenewal {
		if err := l.verifyLease(l.leaseRecord); err == nil {
			return l.leaseRecord, nil
		}
	}

	renewed := false
	for !renewed {
		if err := l.renewLease(); err != nil {
			return nil, err
		}
		if err := l.verifyLease(l.leaseRecord); err == nil {
			renewed = true
		}
	}
	return l.leaseRecord, nil
}

func (l *Lease) keepLeaseAlive() {
	for {
		select {
		case <-l.quitLease:
			return
		case <-time.After(l.timeUntilLeaseRenewal):
			l.ensureValidLease(true /*forceRenewal*/)
		}
	}
}

func (l *Lease) Valid() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if err := l.verifyLease(l.leaseRecord); err == nil {
		return true
	}
	return false
}