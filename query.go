package dht

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	pstore "github.com/libp2p/go-libp2p-core/peerstore"
	"github.com/libp2p/go-libp2p-core/routing"
	kbucket "github.com/libp2p/go-libp2p-kbucket"

	"github.com/libp2p/go-libp2p-kad-dht/qpeerset"
	kb "github.com/libp2p/go-libp2p-kbucket"
)

// ErrNoPeersQueried is returned when we failed to connect to any peers.
var ErrNoPeersQueried = errors.New("failed to query any peers")

type queryFn func(context.Context, peer.ID) ([]*peer.AddrInfo, error)
type stopFn func() bool

// query represents a single DHT query.
type query struct {
	// unique identifier for the lookup instance
	id uuid.UUID

	// target key for the lookup
	key kbucket.ID

	// the query context.
	ctx context.Context

	// the cancellation function for the query context.
	cancel context.CancelFunc

	dht *IpfsDHT

	// seedPeers is the set of peers that seed the query
	seedPeers []peer.ID

	// queryPeers is the set of peers known by this query and their respective states.
	queryPeers *qpeerset.QueryPeerset

	// terminated is set when the first worker thread encounters the termination condition.
	// Its role is to make sure that once termination is determined, it is sticky.
	terminated bool

	// the function that will be used to query a single peer.
	queryFn queryFn

	// stopFn is used to determine if we should stop the WHOLE disjoint query.
	stopFn stopFn
}

type lookupWithFollowupResult struct {
	peers []peer.ID            // the top K not unreachable peers at the end of the query
	state []qpeerset.PeerState // the peer states at the end of the query

	// indicates that neither the lookup nor the followup has been prematurely terminated by an external condition such
	// as context cancellation or the stop function being called.
	completed bool
}

// runLookupWithFollowup executes the lookup on the target using the given query function and stopping when either the
// context is cancelled or the stop function returns true. Note: if the stop function is not sticky, i.e. it does not
// return true every time after the first time it returns true, it is not guaranteed to cause a stop to occur just
// because it momentarily returns true.
//
// After the lookup is complete the query function is run (unless stopped) against all of the top K peers from the
// lookup that have not already been successfully queried.
func (dht *IpfsDHT) runLookupWithFollowup(ctx context.Context, target string, queryFn queryFn, stopFn stopFn) (*lookupWithFollowupResult, error) {
	// run the query
	lookupRes, err := dht.runQuery(ctx, target, queryFn, stopFn)
	if err != nil {
		return nil, err
	}

	// query all of the top K peers we've either Heard about or have outstanding queries we're Waiting on.
	// This ensures that all of the top K results have been queried which adds to resiliency against churn for query
	// functions that carry state (e.g. FindProviders and GetValue) as well as establish connections that are needed
	// by stateless query functions (e.g. GetClosestPeers and therefore Provide and PutValue)
	queryPeers := make([]peer.ID, 0, len(lookupRes.peers))
	for i, p := range lookupRes.peers {
		if state := lookupRes.state[i]; state == qpeerset.PeerHeard || state == qpeerset.PeerWaiting {
			queryPeers = append(queryPeers, p)
		}
	}

	if len(queryPeers) == 0 {
		return lookupRes, nil
	}

	// return if the lookup has been externally stopped
	if ctx.Err() != nil || stopFn() {
		lookupRes.completed = false
		return lookupRes, nil
	}

	doneCh := make(chan struct{}, len(queryPeers))
	followUpCtx, cancelFollowUp := context.WithCancel(ctx)
	for _, p := range queryPeers {
		qp := p
		go func() {
			_, _ = queryFn(followUpCtx, qp)
			doneCh <- struct{}{}
		}()
	}

	// wait for all queries to complete before returning, aborting ongoing queries if we've been externally stopped
processFollowUp:
	for i := 0; i < len(queryPeers); i++ {
		select {
		case <-doneCh:
			if stopFn() {
				cancelFollowUp()
				if i < len(queryPeers)-1 {
					lookupRes.completed = false
				}
				break processFollowUp
			}
		case <-ctx.Done():
			lookupRes.completed = false
			break processFollowUp
		}
	}

	return lookupRes, nil
}

func (dht *IpfsDHT) runQuery(ctx context.Context, target string, queryFn queryFn, stopFn stopFn) (*lookupWithFollowupResult, error) {
	queryCtx, cancelQuery := context.WithCancel(ctx)

	// pick the K closest peers to the key in our Routing table and shuffle them.
	targetKadID := kb.ConvertKey(target)
	seedPeers := dht.routingTable.NearestPeers(targetKadID, dht.bucketSize)
	if len(seedPeers) == 0 {
		routing.PublishQueryEvent(ctx, &routing.QueryEvent{
			Type:  routing.QueryError,
			Extra: kb.ErrLookupFailure.Error(),
		})
		return nil, kb.ErrLookupFailure
	}

	q := &query{
		id:         uuid.New(),
		key:        targetKadID,
		ctx:        queryCtx,
		cancel:     cancelQuery,
		dht:        dht,
		queryPeers: qpeerset.NewQueryPeerset(target),
		seedPeers:  seedPeers,
		terminated: false,
		queryFn:    queryFn,
		stopFn:     stopFn,
	}

	// run the query
	q.run()

	if ctx.Err() != nil {
		q.recordValuablePeers()
	}

	res := q.constructLookupResult(targetKadID)
	return res, nil
}

func recordPeerIsValuable(p peer.ID) {}

func (q *query) recordValuablePeers() {
	closePeers := q.queryPeers.GetClosestNotUnreachable(q.dht.beta)
	for _, p := range closePeers {
		referrer := p
		recordPeerIsValuable(referrer)
		for referrer = q.queryPeers.GetReferrer(referrer); referrer != ""; {
			recordPeerIsValuable(referrer)
		}
	}
}

// constructLookupResult takes the query information and uses it to construct the lookup result
func (q *query) constructLookupResult(target kb.ID) *lookupWithFollowupResult {
	// determine if the query terminated early
	completed := true

	// Lookup and starvation are both valid ways for a lookup to complete. (Starvation does not imply failure.)
	// Lookup termination (as defined in isLookupTermination) is not possible in small networks.
	// Starvation is a successful query termination in small networks.
	if !(q.isLookupTermination() || q.isStarvationTermination()) {
		completed = false
	}

	// extract the top K not unreachable peers
	var peers []peer.ID
	peerState := make(map[peer.ID]qpeerset.PeerState)
	qp := q.queryPeers.GetClosestNotUnreachable(q.dht.bucketSize)
	for _, p := range qp {
		state := q.queryPeers.GetState(p)
		peerState[p] = state
		peers = append(peers, p)
	}

	// get the top K overall peers
	sortedPeers := kb.SortClosestPeers(peers, target)
	if len(sortedPeers) > q.dht.bucketSize {
		sortedPeers = sortedPeers[:q.dht.bucketSize]
	}

	// return the top K not unreachable peers as well as their states at the end of the query
	res := &lookupWithFollowupResult{
		peers:     sortedPeers,
		state:     make([]qpeerset.PeerState, len(sortedPeers)),
		completed: completed,
	}

	for i, p := range sortedPeers {
		res.state[i] = peerState[p]
	}

	return res
}

type queryUpdate struct {
	cause       peer.ID
	heard       []peer.ID
	queried     []peer.ID
	unreachable []peer.ID
}

func (q *query) run() {
	pathCtx, cancelPath := context.WithCancel(q.ctx)
	defer cancelPath()

	alpha := q.dht.alpha

	ch := make(chan *queryUpdate, alpha)
	ch <- &queryUpdate{heard: q.seedPeers}

	for {
		var cause peer.ID
		select {
		case update := <-ch:
			q.updateState(pathCtx, update)
			cause = update.cause
		case <-pathCtx.Done():
			q.terminate(pathCtx, cancelPath, LookupCancelled)
		}

		// termination is triggered on end-of-lookup conditions or starvation of unused peers
		if ready, reason := q.isReadyToTerminate(); ready {
			q.terminate(pathCtx, cancelPath, reason)
		}

		if q.terminated {
			// exit once all outstanding queries have completed.
			// This is important because the queries write to channels provided by the user.
			// The user will close this channels as soon as the lookup completes (i.e. this function returns).
			if q.queryPeers.NumWaiting() == 0 {
				return
			}
			continue
		}

		// if all "threads" are busy, wait until someone finishes
		if q.queryPeers.NumWaiting() >= alpha {
			continue
		}

		// spawn new queries, up to the parallelism allowance
		for j := 0; j < alpha-q.queryPeers.NumWaiting(); j++ {
			q.spawnQuery(pathCtx, cause, ch)
		}
	}
}

// spawnQuery starts one query, if an available heard peer is found
func (q *query) spawnQuery(ctx context.Context, cause peer.ID, ch chan<- *queryUpdate) {
	if peers := q.queryPeers.GetSortedHeard(); len(peers) == 0 {
		return
	} else {
		PublishLookupEvent(ctx, &LookupEvent{
			ID:  q.id,
			Key: q.key,
			Request: &LookupUpdateEvent{
				Cause:   cause,
				Source:  q.queryPeers.GetReferrer(peers[0]),
				Waiting: []peer.ID{peers[0]},
			},
		})
		q.queryPeers.SetState(peers[0], qpeerset.PeerWaiting)
		go q.queryPeer(ctx, ch, peers[0])
	}
}

func (q *query) isReadyToTerminate() (bool, LookupTerminationReason) {
	// give the application logic a chance to terminate
	if q.stopFn() {
		return true, LookupStopped
	}
	if q.isStarvationTermination() {
		return true, LookupStarvation
	}
	if q.isLookupTermination() {
		return true, LookupCompleted
	}
	return false, -1
}

// From the set of all nodes that are not unreachable,
// if the closest beta nodes are all queried, the lookup can terminate.
func (q *query) isLookupTermination() bool {
	var peers []peer.ID
	peers = q.queryPeers.GetClosestNotUnreachable(q.dht.beta)
	for _, p := range peers {
		if q.queryPeers.GetState(p) != qpeerset.PeerQueried {
			return false
		}
	}
	return true
}

func (q *query) isStarvationTermination() bool {
	return q.queryPeers.NumHeard() == 0 && q.queryPeers.NumWaiting() == 0
}

func (q *query) terminate(ctx context.Context, cancel context.CancelFunc, reason LookupTerminationReason) {
	if q.terminated {
		return
	} else {
		PublishLookupEvent(ctx, &LookupEvent{
			ID:        q.id,
			Key:       q.key,
			Terminate: &LookupTerminateEvent{Reason: reason},
		})
		cancel() // abort outstanding queries
		q.terminated = true
	}
}

// queryPeer queries a single peer and reports its findings on the channel.
// queryPeer does not access the query state in queryPeers!
func (q *query) queryPeer(ctx context.Context, ch chan<- *queryUpdate, p peer.ID) {
	dialCtx, queryCtx := ctx, ctx

	// dial the peer
	if err := q.dht.dialPeer(dialCtx, p); err != nil {
		ch <- &queryUpdate{cause: p, unreachable: []peer.ID{p}}
		return
	}

	// send query RPC to the remote peer
	newPeers, err := q.queryFn(queryCtx, p)
	if err != nil {
		ch <- &queryUpdate{cause: p, unreachable: []peer.ID{p}}
		return
	}

	// process new peers
	saw := []peer.ID{}
	for _, next := range newPeers {
		if next.ID == q.dht.self { // don't add self.
			logger.Debugf("PEERS CLOSER -- worker for: %v found self", p)
			continue
		}

		// add their addresses to the dialer's peerstore
		q.dht.peerstore.AddAddrs(next.ID, next.Addrs, pstore.TempAddrTTL)
		saw = append(saw, next.ID)
	}

	ch <- &queryUpdate{cause: p, heard: saw, queried: []peer.ID{p}}
}

func (q *query) updateState(ctx context.Context, up *queryUpdate) {
	PublishLookupEvent(ctx, &LookupEvent{
		ID:  q.id,
		Key: q.key,
		Response: &LookupUpdateEvent{
			Cause:       up.cause,
			Source:      up.cause,
			Heard:       up.heard,
			Queried:     up.queried,
			Unreachable: up.unreachable,
		},
	})
	for _, p := range up.heard {
		if p == q.dht.self { // don't add self.
			continue
		}
		q.queryPeers.TryAdd(p, up.cause)
	}
	for _, p := range up.queried {
		if p == q.dht.self { // don't add self.
			continue
		}
		if st := q.queryPeers.GetState(p); st == qpeerset.PeerWaiting {
			q.queryPeers.SetState(p, qpeerset.PeerQueried)
		} else {
			panic(fmt.Errorf("kademlia protocol error: tried to transition to the queried state from state %v", st))
		}
	}
	for _, p := range up.unreachable {
		if p == q.dht.self { // don't add self.
			continue
		}
		if st := q.queryPeers.GetState(p); st == qpeerset.PeerWaiting {
			q.queryPeers.SetState(p, qpeerset.PeerUnreachable)
		} else {
			panic(fmt.Errorf("kademlia protocol error: tried to transition to the unreachable state from state %v", st))
		}
	}
}

func (dht *IpfsDHT) dialPeer(ctx context.Context, p peer.ID) error {
	// short-circuit if we're already connected.
	if dht.host.Network().Connectedness(p) == network.Connected {
		return nil
	}

	logger.Debug("not connected. dialing.")
	routing.PublishQueryEvent(ctx, &routing.QueryEvent{
		Type: routing.DialingPeer,
		ID:   p,
	})

	pi := peer.AddrInfo{ID: p}
	if err := dht.host.Connect(ctx, pi); err != nil {
		logger.Debugf("error connecting: %s", err)
		routing.PublishQueryEvent(ctx, &routing.QueryEvent{
			Type:  routing.QueryError,
			Extra: err.Error(),
			ID:    p,
		})

		return err
	}
	logger.Debugf("connected. dial success.")
	return nil
}
