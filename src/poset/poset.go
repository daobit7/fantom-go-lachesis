package poset

import (
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"

	"github.com/hashicorp/golang-lru"
	"github.com/sirupsen/logrus"

	"github.com/Fantom-foundation/go-lachesis/src/common"
	"github.com/Fantom-foundation/go-lachesis/src/hash"
	"github.com/Fantom-foundation/go-lachesis/src/inter"
	"github.com/Fantom-foundation/go-lachesis/src/log"
	"github.com/Fantom-foundation/go-lachesis/src/peers"
	"github.com/Fantom-foundation/go-lachesis/src/state"
)

// Core is an interface for interacting with a core.
type Core interface {
	Head() EventHash
	HexID() string
}

// Poset is a DAG of Events. It also contains methods to extract a consensus
// order of Events and map them onto a blockchain.
type Poset struct {
	Participants             *peers.Peers      // [public key] => id
	Store                    Store             // store of Events, Rounds, and Blocks
	UndeterminedEvents       []EventHash       // [index] => hash . FIFO queue of Events whose consensus order is not yet determined
	PendingRounds            []*pendingRound   // FIFO queue of Rounds which have not attained consensus yet
	PendingRoundReceived     common.Int64Slice // FIFO queue of RoundReceived which have not been made into frames yet
	LastConsensusRound       *int64            // index of last consensus round
	FirstConsensusRound      *int64            // index of first consensus round (only used in tests)
	AnchorBlock              *int64            // index of last block with enough signatures
	LastCommittedRoundEvents int               // number of events in round before LastConsensusRound
	SigPool                  []BlockSignature  // Pool of Block signatures that need to be processed
	ConsensusTransactions    uint64            // number of consensus transactions
	pendingLoadedEvents      int64             // number of loaded events that are not yet committed
	commitCh                 chan Block        // channel for committing Blocks
	topologicalIndex         int64             // counter used to order events in topological order (only local)
	superMajority            int
	trustCount               int
	core                     Core

	dominatorCache         *lru.Cache
	selfDominatorCache     *lru.Cache
	strictlyDominatedCache *lru.Cache
	roundCache             *lru.Cache
	timestampCache         *lru.Cache

	logger *logrus.Entry

	undeterminedEventsLocker      sync.RWMutex
	pendingLoadedEventsLocker     sync.RWMutex
	firstLastConsensusRoundLocker sync.RWMutex
	consensusTransactionsLocker   sync.RWMutex
}

// NewPoset instantiates a Poset from a list of participants, underlying
// data store and commit channel
func NewPoset(participants *peers.Peers, store Store, commitCh chan Block, logger *logrus.Entry) *Poset {
	if logger == nil {
		log := logrus.New()
		log.Level = logrus.DebugLevel
		lachesis_log.NewLocal(log, log.Level.String())
		logger = logrus.NewEntry(log)
	}

	superMajority := 2*participants.Len()/3 + 1
	trustCount := int(math.Ceil(float64(participants.Len()) / float64(3)))

	cacheSize := store.CacheSize()
	dominatorCache, err := lru.New(cacheSize)
	if err != nil {
		logger.Fatal("Unable to init Poset.dominatorCache")
	}
	selfDominatorCache, err := lru.New(cacheSize)
	if err != nil {
		logger.Fatal("Unable to init Poset.selfDominatorCache")
	}
	strictlyDominatedCache, err := lru.New(cacheSize)
	if err != nil {
		logger.Fatal("Unable to init Poset.strictlyDominatedCache")
	}
	roundCache, err := lru.New(cacheSize)
	if err != nil {
		logger.Fatal("Unable to init Poset.roundCreatedCache")
	}
	timestampCache, err := lru.New(cacheSize)
	if err != nil {
		logger.Fatal("Unable to init Poset.timestampCache")
	}
	poset := Poset{
		Participants:           participants,
		Store:                  store,
		PendingRounds:          []*pendingRound{},
		PendingRoundReceived:   common.Int64Slice{},
		commitCh:               commitCh,
		dominatorCache:         dominatorCache,
		selfDominatorCache:     selfDominatorCache,
		strictlyDominatedCache: strictlyDominatedCache,
		roundCache:             roundCache,
		timestampCache:         timestampCache,
		logger:                 logger,
		superMajority:          superMajority,
		trustCount:             trustCount,
	}

	// Leaf events are roots by default, so we need to construct a common
	// flagtable indicating leaf events can see each other.
	ft := FlagTable{}
	for selfParentHash := range poset.Store.RootsBySelfParent() {
		ft[selfParentHash] = 1
	}
	// Set Leaf Events for each participant
	for participant, root := range poset.Store.RootsByParticipant() {
		var creator []byte
		if _, err := fmt.Sscanf(participant, "0x%X", &creator); err != nil {
			panic(err)
		}
		body := EventBody{
			Creator: creator,
			Index:   0,
			Parents: EventHashes{EventHash{}, EventHash{}}.Bytes(),
		}
		event := Event{
			Message: &EventMessage{
				Hash:             root.SelfParent.Hash,
				CreatorID:        root.SelfParent.CreatorID,
				TopologicalIndex: -1,
				Body:             &body,
				FlagTable:        ft.Marshal(),
				ClothoProof:      [][]byte{root.SelfParent.Hash},
			},
			lamportTimestamp: 0,
			round:            0,
			roundReceived:    0, /*RoundNIL*/
		}
		if err := poset.Store.SetEvent(event); err != nil {
			panic(err)
		}
	}

	participants.OnNewPeer(func(peer *peers.Peer) {
		poset.superMajority = 2*participants.Len()/3 + 1
		poset.trustCount = int(math.Ceil(float64(participants.Len()) / float64(3)))
	})

	return &poset
}

// SetCore sets a core for poset.
func (p *Poset) SetCore(core Core) {
	p.core = core
}

/*******************************************************************************
Private Methods
*******************************************************************************/

// true if y is an dominator of x
func (p *Poset) dominator(x, y EventHash) (bool, error) {
	if c, ok := p.dominatorCache.Get(Key{x, y}); ok {
		return c.(bool), nil
	}

	if len(x) == 0 || len(y) == 0 {
		return false, nil
	}

	a, err := p.dominator2(x, y)
	if err != nil {
		return false, err
	}
	p.dominatorCache.Add(Key{x, y}, a)
	return a, nil
}

func (p *Poset) dominator2(x, y EventHash) (bool, error) {
	if x == y {
		return true, nil
	}

	roots := p.Store.RootsBySelfParent()

	ex, err := p.Store.GetEventBlock(x)
	if err != nil {
		for _, root := range roots {
			if other, ok := root.Others[y.String()]; ok {
				return x.Equal(other.Hash), nil
			}
		}
		return false, nil
	}
	if lamportDiff, err := p.lamportTimestampDiff(x, y); err != nil || lamportDiff > 0 {
		return false, err
	}

	ey, err := p.Store.GetEventBlock(y)
	if err != nil {
		// check y roots
		if root, ok := roots[y]; ok {
			peer, ok := p.Participants.ReadByID(root.SelfParent.CreatorID)
			if !ok {
				return false, fmt.Errorf("creator with ID %v not found", root.SelfParent.CreatorID)
			}
			yCreator := peer.PubKeyHex
			if ex.GetCreator() == yCreator {
				return ex.Index() >= root.SelfParent.Index, nil
			}
		} else {
			return false, nil
		}
	} else {
		// check if creators are equals and check indexes
		if ex.GetCreator() == ey.GetCreator() {
			return ex.Index() >= ey.Index(), nil
		}
	}

	res, err := p.dominator(ex.SelfParent(), y)
	if err != nil {
		return false, err
	}

	if res {
		return true, nil
	}

	return p.dominator(ex.OtherParent(), y)
}

// true if y is a self-dominator of x
func (p *Poset) selfDominator(x, y EventHash) (bool, error) {
	if c, ok := p.selfDominatorCache.Get(Key{x, y}); ok {
		return c.(bool), nil
	}
	if len(x) == 0 || len(y) == 0 {
		return false, nil
	}
	a, err := p.selfDominator2(x, y)
	if err != nil {
		return false, err
	}
	p.selfDominatorCache.Add(Key{x, y}, a)
	return a, nil
}

func (p *Poset) selfDominator2(x, y EventHash) (bool, error) {
	if x == y {
		return true, nil
	}
	roots := p.Store.RootsBySelfParent()
	ex, err := p.Store.GetEventBlock(x)
	if err != nil {
		if root, ok := roots[x]; ok {
			if y.Equal(root.SelfParent.Hash) {
				return true, nil
			}
		}
		return false, err
	}

	ey, err := p.Store.GetEventBlock(y)
	if err != nil {
		if root, ok := roots[y]; ok {
			peer, ok := p.Participants.ReadByID(root.SelfParent.CreatorID)
			if !ok {
				return false, fmt.Errorf("self-parent creator with ID %v not found", root.SelfParent.CreatorID)
			}
			yCreator := peer.PubKeyHex
			if ex.GetCreator() == yCreator {
				return ex.Index() >= root.SelfParent.Index, nil
			}
		}
	} else {
		if ex.GetCreator() == ey.GetCreator() {
			return ex.Index() >= ey.Index(), nil
		}
	}

	return false, nil
}

// true if x is dominated by y
func (p *Poset) dominated(x, y EventHash) (bool, error) {
	return p.dominator(x, y)
	// it is not necessary to detect forks because we assume that the InsertEvent
	// function makes it impossible to insert two Events at the same height for
	// the same participant.
}

// true if x strictly dominated by y
func (p *Poset) strictlyDominated(x, y EventHash) (bool, error) {
	if len(x) == 0 || len(y) == 0 {
		return false, nil
	}

	if c, ok := p.strictlyDominatedCache.Get(Key{x, y}); ok {
		return c.(bool), nil
	}
	ss, err := p.strictlyDominated2(x, y)
	if err != nil {
		return false, err
	}
	p.strictlyDominatedCache.Add(Key{x, y}, ss)
	return ss, nil
}

// Possible improvement: Populate the cache for upper and downer events
// that also strictlyDominates y
func (p *Poset) strictlyDominated2(x, y EventHash) (bool, error) {
	sentinels := make(map[string]bool)

	if err := p.MapSentinels(x, y, sentinels); err != nil {
		return false, err
	}

	return len(sentinels) >= p.superMajority, nil
}

// MapSentinels participants in x's dominator that dominate y
func (p *Poset) MapSentinels(x, y EventHash, sentinels map[string]bool) error {
	if x.Zero() {
		return nil
	}

	if dominated, err := p.dominated(x, y); err != nil || !dominated {
		return err
	}

	ex, err := p.Store.GetEventBlock(x)

	if err != nil {
		roots := p.Store.RootsBySelfParent()

		if root, ok := roots[x]; ok {
			creator, ok := p.Participants.ReadByID(root.SelfParent.CreatorID)
			if !ok {
				return fmt.Errorf("self-parent creator with ID %v not found", root.SelfParent.CreatorID)
			}

			sentinels[creator.PubKeyHex] = true

			return nil
		}

		return err
	}

	creator, ok := p.Participants.ReadByID(ex.CreatorID())
	if !ok {
		return fmt.Errorf("creator with ID %v not found", ex.CreatorID())
	}
	sentinels[creator.PubKeyHex] = true

	if x == y {
		return nil
	}

	if err := p.MapSentinels(ex.OtherParent(), y, sentinels); err != nil {
		return err
	}

	return p.MapSentinels(ex.SelfParent(), y, sentinels)
}

func (p *Poset) round(x EventHash) (int64, error) {
	if c, ok := p.roundCache.Get(x); ok {
		return c.(int64), nil
	}
	r, err := p.round2(x)
	if err != nil {
		return -1, err
	}
	p.roundCache.Add(x, r)
	return r, nil
}

func (p *Poset) round2(x EventHash) (int64, error) {

	/*
		x is the Root
		Use Root.SelfParent.Round
	*/
	rootsBySelfParent := p.Store.RootsBySelfParent()
	if r, ok := rootsBySelfParent[x]; ok {
		p.logger.Debug("p.round2(): return r.SelfParent.Round")
		return r.SelfParent.Round, nil
	}

	ex, err := p.Store.GetEventBlock(x)
	if err != nil {
		p.logger.Debug("p.round2(): return math.MinInt64")
		return math.MinInt64, err
	}

	root, err := p.Store.GetRoot(ex.GetCreator())
	if err != nil {
		p.logger.Debug("p.round2(): return math.MinInt64 2")
		return math.MinInt64, err
	}

	/*
		The Event is directly attached to the Root.
	*/
	if sp := ex.SelfParent(); sp.Equal(root.SelfParent.Hash) {
		// Root is authoritative EXCEPT if other-parent is not in the root
		hash := ex.Hash()
		op := ex.OtherParent()
		if other, ok := root.Others[hash.String()]; op.Zero() ||
			(ok && op.Equal(other.Hash)) {

			p.logger.Debug("p.round2(): return root.NextRound")
			return root.NextRound, nil
		}
	}

	/*
		The Event's parents are "normal" Events.
		Use the whitepaper formula: parentRound + roundInc
	*/
	spRound, err := p.round(ex.SelfParent())
	if err != nil {
		p.logger.Debug("p.round2(): return RoundNIL")
		return RoundNIL, err
	}
	var parentRound = spRound
	opRound, err := p.round(ex.OtherParent())
	if err != nil {
		p.logger.Debug("p.round2(): return RoundNIL 2")
		return RoundNIL, err
	}
	if opRound > parentRound {
		parentRound = opRound
	}
	p.logger.WithField("parentRound", parentRound).Debug("p.round2()")

	// base of recursion. If both parents are of RoundNIL they are leaf events
	if parentRound == RoundNIL {
		return 0, nil
	}

	ws := p.Store.RoundClothos(parentRound)
	p.logger.WithFields(logrus.Fields{
		"len(ws)": len(ws),
	}).Debug("p.round2()")

	isDominated := func(poset *Poset, root EventHash, clothos EventHashes) bool {
		for _, w := range ws {
			if w == root && w != ex.Hash() {
				dominate, err := poset.dominated(ex.Hash(), w)
				if err != nil {
					return false
				}
				if dominate {
					return true
				}
			}
		}
		return false
	}

	// check wp
	p.logger.WithFields(logrus.Fields{
		"len(ex.Message.ClothoProof)": len(ex.Message.ClothoProof),
		"p.superMajority":             p.superMajority,
	}).Debug("p.round2()")
	if len(ex.Message.ClothoProof) >= p.superMajority {
		count := 0
		for _, h := range ex.Message.ClothoProof {
			var root EventHash
			root.Set(h)
			if isDominated(p, root, ws) {
				count++
			}
		}

		p.logger.WithFields(logrus.Fields{
			"len(ex.Message.ClothoProof)": len(ex.Message.ClothoProof),
			"p.superMajority":             p.superMajority,
			"count":                       count,
		}).Debug("p.round2()")
		if count >= p.superMajority {
			p.logger.Debug("p.round2(): return parentRound + 1")
			return parentRound + 1, err
		}
	} else {

		// check ft
		ft, _ := ex.GetFlagTable()
		p.logger.WithFields(logrus.Fields{
			"len(ft)":         len(ft),
			"p.superMajority": p.superMajority,
		}).Debug("p.round2()")
		if len(ft) >= p.superMajority {
			count := 0

			for root := range ft {
				if isDominated(p, root, ws) {
					count++
				}
			}

			p.logger.WithFields(logrus.Fields{
				"len(ft)":         len(ft),
				"count":           count,
				"p.superMajority": p.superMajority,
			}).Debug("p.round2()")
			if count >= p.superMajority {
				p.logger.Debug("p.round2(): return parentRound + 1 (2)")
				return parentRound + 1, err
			}
		}
	}
	p.logger.Debug("p.round2(): return parentRound, last")
	return parentRound, nil
}

// clotho if is true then x is a clotho (first event of a round for the owner)
func (p *Poset) clotho(x EventHash) (bool, error) {
	ex, err := p.Store.GetEventBlock(x)
	if err != nil {
		return false, err
	}

	xRound, err := p.round(x)
	if err != nil {
		return false, err
	}
	spRound, err := p.round(ex.SelfParent())
	if err != nil {
		return false, err
	}
	return xRound > spRound, nil
}

func (p *Poset) lamportTimestamp(x EventHash) (int64, error) {
	if c, ok := p.timestampCache.Get(x); ok {
		return c.(int64), nil
	}
	r, err := p.lamportTimestamp2(x)
	if err != nil {
		return -1, err
	}
	p.timestampCache.Add(x, r)
	return r, nil
}

func (p *Poset) lamportTimestamp2(x EventHash) (int64, error) {
	/*
		x is the Root
		User Root.SelfParent.LamportTimestamp
	*/
	rootsBySelfParent := p.Store.RootsBySelfParent()
	if r, ok := rootsBySelfParent[x]; ok {
		return r.SelfParent.LamportTimestamp, nil
	}

	ex, err := p.Store.GetEventBlock(x)
	if err != nil {
		return math.MinInt64, err
	}

	// We are going to need the Root later
	root, err := p.Store.GetRoot(ex.GetCreator())
	if err != nil {
		return math.MinInt64, err
	}

	var plt int64
	// If it is the creator's first Event, use the corresponding Root
	selfParent := ex.SelfParent()
	if selfParent.Equal(root.SelfParent.Hash) {
		plt = root.SelfParent.LamportTimestamp
	} else {
		t, err := p.lamportTimestamp(selfParent)
		if err != nil {
			return math.MinInt64, err
		}
		plt = t
	}

	otherParent := ex.OtherParent()
	if !otherParent.Zero() {
		opLT := int64(math.MinInt64)
		if _, err := p.Store.GetEventBlock(otherParent); err == nil {
			// if we know the other-parent, fetch its Round directly
			t, err := p.lamportTimestamp(otherParent)
			if err != nil {
				return math.MinInt64, err
			}
			opLT = t
		} else if other, ok := root.Others[x.String()]; ok && otherParent.Equal(other.Hash) {
			// we do not know the other-parent but it is referenced  in Root.Others
			// we use the Root's LamportTimestamp
			opLT = other.LamportTimestamp
		}

		if opLT > plt {
			plt = opLT
		}
	}

	return plt + 1, nil
}

// lamport(y) - lamport(x)
func (p *Poset) lamportTimestampDiff(x, y EventHash) (int64, error) {
	xlt, err := p.lamportTimestamp(x)
	if err != nil {
		return 0, err
	}
	ylt, err := p.lamportTimestamp(y)
	if err != nil {
		return 0, err
	}
	return ylt - xlt, nil
}

// round(x) - round(y)
func (p *Poset) roundDiff(x, y EventHash) (int64, error) {

	xRound, err := p.round(x)
	if err != nil {
		return math.MinInt64, fmt.Errorf("event %s has negative round", x)
	}

	yRound, err := p.round(y)
	if err != nil {
		return math.MinInt64, fmt.Errorf("event %s has negative round", y)
	}

	return xRound - yRound, nil
}

// Check the SelfParent is the Creator's last known Event
func (p *Poset) checkSelfParent(event Event) error {
	selfParent := event.SelfParent()
	creator := event.GetCreator()

	creatorLastKnown, _, err := p.Store.LastEventFrom(creator)

	p.logger.WithFields(logrus.Fields{
		"selfParent":       selfParent,
		"creator":          creator,
		"creatorLastKnown": creatorLastKnown,
		"event":            event.Hash(),
	}).Debugf("checkSelfParent")

	if err != nil {
		return err
	}

	selfParentLegit := selfParent == creatorLastKnown

	if !selfParentLegit {
		return fmt.Errorf("self-parent not last known event by creator")
	}

	return nil
}

// Check if we know the OtherParent
func (p *Poset) checkOtherParent(event Event) error {
	otherParent := event.OtherParent()
	if !otherParent.Zero() {
		// Check if we have it
		_, err := p.Store.GetEventBlock(otherParent)
		if err != nil {
			// it might still be in the Root
			root, err := p.Store.GetRoot(event.GetCreator())
			if err != nil {
				return err
			}
			hash := event.Hash()
			otherParent := event.OtherParent()
			other, ok := root.Others[hash.String()]
			if ok && otherParent.Equal(other.Hash) {
				return nil
			}
			return fmt.Errorf("other-parent not known")
		}
	}
	return nil
}

func (p *Poset) createSelfParentRootEvent(ev Event) (RootEvent, error) {
	sp := ev.SelfParent()
	spLT, err := p.lamportTimestamp(sp)
	if err != nil {
		return RootEvent{}, err
	}
	spRound, err := p.round(sp)
	if err != nil {
		return RootEvent{}, err
	}
	peer, ok := p.Participants.ReadByPubKey(ev.GetCreator())
	if !ok {
		return RootEvent{}, fmt.Errorf("creator %v not found", ev.GetCreator())
	}
	selfParentRootEvent := RootEvent{
		Hash:             sp.Bytes(),
		CreatorID:        peer.ID,
		Index:            ev.Index() - 1,
		LamportTimestamp: spLT,
		Round:            spRound,
		// FlagTable:ev.FlagTable,
		// flags:ev.flags,
	}
	return selfParentRootEvent, nil
}

func (p *Poset) createOtherParentRootEvent(ev Event) (RootEvent, error) {
	op := ev.OtherParent()

	// it might still be in the Root
	root, err := p.Store.GetRoot(ev.GetCreator())
	if err != nil {
		return RootEvent{}, err
	}
	hash := ev.Hash()
	if other, ok := root.Others[hash.String()]; ok && op.Equal(other.Hash) {
		return *other, nil
	}

	otherParent, err := p.Store.GetEventBlock(op)
	if err != nil {
		return RootEvent{}, err
	}
	opLT, err := p.lamportTimestamp(op)
	if err != nil {
		return RootEvent{}, err
	}
	opRound, err := p.round(op)
	if err != nil {
		return RootEvent{}, err
	}
	peer, ok := p.Participants.ReadByPubKey(otherParent.GetCreator())
	if !ok {
		return RootEvent{}, fmt.Errorf("other parent's creator %v not found", otherParent.GetCreator())
	}
	otherParentRootEvent := RootEvent{
		Hash:             op.Bytes(),
		CreatorID:        peer.ID,
		Index:            otherParent.Index(),
		LamportTimestamp: opLT,
		Round:            opRound,
	}
	return otherParentRootEvent, nil

}

func (p *Poset) createRoot(ev Event) (Root, error) {

	evRound, err := p.round(ev.Hash())
	if err != nil {
		return Root{}, err
	}

	/*
		SelfParent
	*/
	selfParentRootEvent, err := p.createSelfParentRootEvent(ev)
	if err != nil {
		return Root{}, err
	}

	/*
		OtherParent
	*/
	var otherParentRootEvent *RootEvent
	otherParent := ev.OtherParent()
	if !otherParent.Zero() {
		opre, err := p.createOtherParentRootEvent(ev)
		if err != nil {
			return Root{}, err
		}
		otherParentRootEvent = &opre
	}

	root := Root{
		NextRound:  evRound,
		SelfParent: &selfParentRootEvent,
		Others:     map[string]*RootEvent{},
	}

	if otherParentRootEvent != nil {
		hash := ev.Hash()
		root.Others[hash.String()] = otherParentRootEvent
	}

	return root, nil
}

// SetWireInfo set wire info for the event
func (p *Poset) SetWireInfo(event *Event) error {
	return p.setWireInfo(event)
}

// SetWireInfoAndSign set wire info for the event and sign
func (p *Poset) SetWireInfoAndSign(event *Event, privKey *ecdsa.PrivateKey) error {
	if err := p.setWireInfo(event); err != nil {
		return err
	}
	return event.Sign(privKey)
}

func (p *Poset) setWireInfo(event *Event) error {

	eventCreator := event.GetCreator()
	creator, ok := p.Participants.ReadByPubKey(eventCreator)
	if !ok {
		return fmt.Errorf("creator %s not found", eventCreator)
	}

	selfParent, err := p.Store.GetEventBlock(event.SelfParent())
	if err != nil {
		return err
	}

	otherParent, err := p.Store.GetEventBlock(event.OtherParent())
	if err != nil {
		return err
	}
	otherParentCreator, ok := p.Participants.ReadByPubKey(otherParent.GetCreator())
	if !ok {
		return fmt.Errorf("creator %s not found", otherParent.GetCreator())
	}

	event.SetWireInfo(selfParent.Index(),
		otherParentCreator.ID,
		otherParent.Index(),
		creator.ID)

	return nil
}

func (p *Poset) updatePendingRounds(decidedRounds map[int64]int64) {
	for _, ur := range p.PendingRounds {
		if _, ok := decidedRounds[ur.Index]; ok {
			ur.Decided = true
		}
	}
}

// Remove processed Signatures from SigPool
func (p *Poset) removeProcessedSignatures(processedSignatures map[int64]bool) {
	var newSigPool []BlockSignature
	for _, bs := range p.SigPool {
		if _, ok := processedSignatures[bs.Index]; !ok {
			newSigPool = append(newSigPool, bs)
		}
	}
	p.SigPool = newSigPool
}

/*******************************************************************************
Public Methods
*******************************************************************************/

// InsertEvent attempts to insert an Event in the DAG. It verifies the signature,
// checks the dominators are known, and prevents the introduction of forks.
func (p *Poset) InsertEvent(event Event, setWireInfo bool) error {
	// verify signature
	if ok, err := event.Verify(); !ok {
		if err != nil {
			return err
		}
		hash := event.Hash()
		p.logger.WithFields(logrus.Fields{
			"event":      event,
			"creator":    event.GetCreator(),
			"selfParent": event.SelfParent(),
			"index":      event.Index(),
			"hex":        hash.String(),
		}).Debugf("Invalid Event signature")

		return fmt.Errorf("invalid Event signature")
	}

	if err := p.checkSelfParent(event); err != nil {
		return fmt.Errorf("CheckSelfParent: %s", err)
	}

	if err := p.checkOtherParent(event); err != nil {
		return fmt.Errorf("CheckOtherParent: %s", err)
	}

	event.Message.TopologicalIndex = p.topologicalIndex
	p.topologicalIndex++

	if setWireInfo {
		if err := p.setWireInfo(&event); err != nil {
			return fmt.Errorf("SetWireInfo: %s", err)
		}
	}

	if err := p.Store.SetEvent(event); err != nil {
		return fmt.Errorf("SetEvent: %s", err)
	}

	p.undeterminedEventsLocker.Lock()
	p.UndeterminedEvents = append(p.UndeterminedEvents, event.Hash())
	p.undeterminedEventsLocker.Unlock()

	if event.IsLoaded() {
		p.pendingLoadedEventsLocker.Lock()
		p.pendingLoadedEvents++
		p.pendingLoadedEventsLocker.Unlock()
	}

	blockSignatures := make([]BlockSignature, len(event.BlockSignatures()))
	for i, v := range event.BlockSignatures() {
		blockSignatures[i] = *v
	}
	p.SigPool = append(p.SigPool, blockSignatures...)

	return nil
}

/*
DivideRounds assigns a Round and LamportTimestamp to Events, and flags them as
clothos if necessary. Pushes Rounds in the PendingRounds queue if necessary.
*/
func (p *Poset) DivideRounds() error {

	p.undeterminedEventsLocker.RLock()
	defer p.undeterminedEventsLocker.RUnlock()

	for _, hash := range p.UndeterminedEvents {

		ev, err := p.Store.GetEventBlock(hash)
		if err != nil {
			return err
		}

		updateEvent := false

		/*
		   Compute Event's round, update the corresponding Round object, and
		   add it to the PendingRounds queue if necessary.
		*/
		if ev.GetRound() == RoundNIL {

			roundNumber, err := p.round(hash)
			if err != nil {
				return err
			}

			ev.SetRound(roundNumber)
			updateEvent = true

			roundCreated, err := p.Store.GetRoundCreated(roundNumber)
			if err != nil && !common.Is(err, common.KeyNotFound) {
				return err
			}

			p.logger.WithFields(logrus.Fields{
				"hash":         hash,
				"roundNumber":  roundNumber,
				"roundCreated": roundCreated,
			}).Debug("p.DivideRounds()")

			/*
				Why the lower bound?
				Normally, once a Round has attained consensus, it is impossible for
				new Events from a previous Round to be inserted; the lower bound
				appears redundant. This is the case when the poset grows
				linearly, without jumps, which is what we intend by 'Normally'.
				But the Reset function introduces a discontinuity  by jumping
				straight to a specific place in the poset. This technique relies
				on a base layer of Events (the corresponding Frame's Events) for
				other Events to be added on top, but the base layer must not be
				reprocessed.
			*/
			if !roundCreated.Message.Queued && roundNumber >= p.GetLastConsensusRound() {

				p.PendingRounds = append(p.PendingRounds, &pendingRound{roundNumber, false})
				roundCreated.Message.Queued = true
			}

			clotho, err := p.clotho(hash)
			if err != nil {
				return err
			}
			roundCreated.AddEvent(hash, clotho)

			err = p.Store.SetRoundCreated(roundNumber, roundCreated)
			if err != nil {
				return err
			}

			if clotho {
				// if event is self head
				if p.core != nil && ev.Hash() == p.core.Head() &&
					ev.GetCreator() == p.core.HexID() {

					replaceFlagTable := func(event *Event, round int64) {
						ft := make(FlagTable)
						ws := p.Store.RoundClothos(round)
						for _, v := range ws {
							ft[v] = 1
						}
						if err := event.ReplaceFlagTable(ft); err != nil {
							p.logger.Fatal(err)
						}
					}

					// special case
					if ev.GetRound() == 0 {
						replaceFlagTable(&ev, 0)
						root, err := p.Store.GetRoot(ev.GetCreator())
						if err != nil {
							return err
						}
						ev.Message.ClothoProof = [][]byte{root.SelfParent.Hash}
					} else {
						replaceFlagTable(&ev, ev.GetRound())
						roots := p.Store.RoundClothos(ev.GetRound() - 1)
						ev.Message.ClothoProof = roots.Bytes()
					}
				}
			}
		}

		/*
			Compute the Event's LamportTimestamp
		*/
		if ev.GetLamportTimestamp() == LamportTimestampNIL {

			lamportTimestamp, err := p.lamportTimestamp(hash)
			if err != nil {
				return err
			}

			ev.SetLamportTimestamp(lamportTimestamp)
			updateEvent = true
		}

		if updateEvent {
			if ev.CreatorID() == 0 {
				if err := p.setWireInfo(&ev); err != nil {
					p.logger.Fatal(err)
				}
			}
			if err := p.Store.SetEvent(ev); err != nil {
				p.logger.Fatal(err)
			}
		}
	}

	return nil
}

// DecideAtropos decides if clothos are atropos
func (p *Poset) DecideAtropos() error {

	// Initialize the vote map
	votes := make(map[EventHash]map[EventHash]bool) // [x][y]=>vote(x,y)
	setVote := func(votes map[EventHash]map[EventHash]bool, x, y EventHash, vote bool) {
		if votes[x] == nil {
			votes[x] = make(map[EventHash]bool)
		}
		votes[x][y] = vote
	}

	decidedRounds := map[int64]int64{} // [round number] => index in p.PendingRounds
	c := 11

	for pos, r := range p.PendingRounds {
		roundIndex := r.Index
		roundInfo, err := p.Store.GetRoundCreated(roundIndex)
		if err != nil {
			return err
		}
		for _, x := range roundInfo.Clotho() {
			if roundInfo.IsDecided(x) {
				continue
			}
		VoteLoop:
			for j := roundIndex + 1; j <= p.Store.LastRound(); j++ {
				for _, y := range p.Store.RoundClothos(j) {
					diff := j - roundIndex
					if diff == 1 {
						ycx, err := p.dominated(y, x)
						if err != nil {
							return err
						}
						setVote(votes, y, x, ycx)
					} else {
						// count votes
						var ssClotho []EventHash
						for _, w := range p.Store.RoundClothos(j - 1) {
							ss, err := p.strictlyDominated(y, w)
							if err != nil {
								return err
							}
							if ss {
								ssClotho = append(ssClotho, w)
							}
						}
						yays := 0
						nays := 0
						for _, w := range ssClotho {
							if votes[w][x] {
								yays++
							} else {
								nays++
							}
						}
						v := false
						t := nays
						if yays >= nays {
							v = true
							t = yays
						}

						// normal round
						if math.Mod(float64(diff), float64(c)) > 0 {
							if t >= p.superMajority {
								roundInfo.SetAtropos(x, v)
								setVote(votes, y, x, v)
								break VoteLoop // break out of j loop
							} else {
								setVote(votes, y, x, v)
							}
						} else { // coin round
							if t >= p.superMajority {
								setVote(votes, y, x, v)
							} else {
								setVote(votes, y, x, randomShift(y)) // middle bit of y's hash
							}
						}
					}
				}
			}
		}

		err = p.Store.SetRoundCreated(roundIndex, roundInfo)
		if err != nil {
			return err
		}

		if roundInfo.ClothoDecided() {
			decidedRounds[roundIndex] = int64(pos)
		}
	}

	p.updatePendingRounds(decidedRounds)
	return nil
}

// DecideRoundReceived assigns a RoundReceived to undetermined events when they
// reach consensus
func (p *Poset) DecideRoundReceived() error {

	p.undeterminedEventsLocker.Lock()
	defer p.undeterminedEventsLocker.Unlock()

	var newUndeterminedEvents []EventHash

	/* From whitepaper - 18/03/18
	   "[...] An event is said to be “received” in the first round where all the
	   unique atropos have received it
	*/

	pendingRoundReceived := map[int64]bool{}

	for _, x := range p.UndeterminedEvents {

		received := false
		r, err := p.round(x)
		if err != nil {
			return err
		}

		for i := r + 1; i <= p.Store.LastRound(); i++ {

			tr, err := p.Store.GetRoundCreated(i)
			if err != nil {
				// Can happen after a Reset/FastSync
				if r < p.GetLastConsensusRound() {
					received = true
					break
				}
				return err
			}

			// We are looping from earlier to later rounds; so if we encounter
			// one round with undecided clothos, we are sure that this event
			// is not "received". Break out of i loop
			if !(tr.ClothoDecided()) {
				break
			}

			fws := tr.Atropos()
			// set of atropos that dominates x
			var s []EventHash
			for _, w := range fws {
				dominates, err := p.dominated(w, x)
				if err != nil {
					return err
				}
				if dominates {
					s = append(s, w)
				}
			}

			if len(s) == len(fws) && len(s) > 0 {

				received = true

				ex, err := p.Store.GetEventBlock(x)
				if err != nil {
					return err
				}
				ex.SetRoundReceived(i)

				err = p.Store.SetEvent(ex)
				if err != nil {
					return err
				}

				tr.SetConsensusEvent(x)
				roundReceived, err := p.Store.GetRoundReceived(i)
				if err != nil {
					roundReceived = *NewRoundReceived()
				}

				roundReceived.Rounds = append(roundReceived.Rounds, x.Bytes())

				err = p.Store.SetRoundReceived(i, roundReceived)
				if err != nil {
					return err
				}

				pendingRoundReceived[i] = true

				// break out of i loop
				break
			}

		}

		if !received {
			newUndeterminedEvents = append(newUndeterminedEvents, x)
		}
	}

	for i := range pendingRoundReceived {
		p.PendingRoundReceived = append(p.PendingRoundReceived, i)
	}

	sort.Sort(p.PendingRoundReceived)

	p.UndeterminedEvents = newUndeterminedEvents

	return nil
}

// ProcessDecidedRounds takes Rounds whose clothos are decided, computes the
// corresponding Frames, maps them into Blocks, and commits the Blocks via the
// commit channel
func (p *Poset) ProcessDecidedRounds() error {

	// Defer removing processed Rounds from the PendingRounds Queue
	processedIndex := 0
	defer func() {
		if processedIndex == 0 {
			return
		}
		lastProcessedRound := p.PendingRoundReceived[processedIndex-1]
		for i, round := range p.PendingRounds {
			if round.Index == lastProcessedRound {
				p.PendingRounds = p.PendingRounds[i+1:]
				break
			}
		}
		p.PendingRoundReceived = p.PendingRoundReceived[processedIndex:]
	}()

	for _, r := range p.PendingRoundReceived {

		// Although it is possible for a Round to be 'decided' before a previous
		// round, we should NEVER process a decided round before all the previous
		// rounds are processed.
		// if !r.Decided {
		// 	break
		// }
		// Don't skip rounds
		if p.LastConsensusRound != nil && r > (*p.LastConsensusRound+1) {
			break
		}

		// This is similar to the lower bound introduced in DivideRounds; it is
		// redundant in normal operations, but becomes necessary after a Reset.
		// Indeed, after a Reset, LastConsensusRound is added to PendingRounds,
		// but its ConsensusEvents (which are necessarily 'under' this Round) are
		// already deemed committed. Hence, skip this Round after a Reset.
		if r == p.GetLastConsensusRound() {
			continue
		}

		frame, err := p.GetFrame(r)
		if err != nil {
			return fmt.Errorf("getting Frame %d: %v", r, err)
		}

		// round, err := p.Store.GetRoundReceived(r)
		// if err != nil {
		// 	return err
		// }
		p.logger.WithFields(logrus.Fields{
			"round_received": r,
			"events":         len(frame.Events),
			"roots":          frame.Roots,
		}).Debugf("Processing Decided Round")

		if len(frame.Events) > 0 {

			for _, e := range frame.Events {
				ev := e.ToEvent()
				err := p.Store.AddConsensusEvent(ev)
				if err != nil {
					return err
				}
				p.consensusTransactionsLocker.Lock()
				p.ConsensusTransactions += uint64(len(ev.Transactions()))
				p.consensusTransactionsLocker.Unlock()
				if ev.IsLoaded() {
					p.pendingLoadedEventsLocker.Lock()
					p.pendingLoadedEvents--
					p.pendingLoadedEventsLocker.Unlock()
				}
			}

			lastBlockIndex := p.Store.LastBlockIndex()
			block, err := NewBlockFromFrame(lastBlockIndex+1, frame)
			if err != nil {
				return err
			}
			if len(block.Transactions()) > 0 {
				if err := p.Store.SetBlock(block); err != nil {
					return err
				}

				if p.commitCh != nil {
					p.commitCh <- block
				}
			}

		} else {
			p.logger.Debugf("No Events to commit for ConsensusRound %d", r)
		}

		processedIndex++

		if p.GetLastConsensusRound() < 0 || r > p.GetLastConsensusRound() {
			p.setLastConsensusRound(r)
		}

	}

	return nil
}

// GetFrame returns the Frame corresponding to a RoundReceived.
func (p *Poset) GetFrame(roundReceived int64) (Frame, error) {
	// Try to get it from the Store first
	frame, err := p.Store.GetFrame(roundReceived)
	if err == nil || !common.Is(err, common.KeyNotFound) {
		return frame, err
	}
	// otherwise make new
	return p.MakeFrame(roundReceived)
}

// MakeFrame computes the Frame corresponding to a RoundReceived.
func (p *Poset) MakeFrame(roundReceived int64) (Frame, error) {
	// Get the Round and corresponding consensus Events
	round, err := p.Store.GetRoundReceived(roundReceived)
	if err != nil {
		return Frame{}, err
	}

	var events []Event
	for _, eh := range round.Rounds {
		var hash EventHash
		hash.Set(eh)
		e, err := p.Store.GetEventBlock(hash)
		if err != nil {
			return Frame{}, err
		}
		events = append(events, e)
	}

	sort.Stable(ByLamportTimestamp(events))

	stateHash, err := p.ApplyInternalTransactions(roundReceived, events)
	if err != nil {
		return Frame{}, err
	}

	// Get/Create Roots
	roots := make(map[string]Root)
	// The events are in topological order. Each time we run into the first Event
	// of a participant, we create a Root for it.
	for _, ev := range events {
		c := ev.GetCreator()
		if _, ok := roots[c]; !ok {
			root, err := p.createRoot(ev)
			if err != nil {
				return Frame{}, err
			}
			roots[ev.GetCreator()] = root
		}
	}

	// Every participant needs a Root in the Frame. For the participants that
	// have no Events in this Frame, we create a Root from their last consensus
	// Event, or their last known Root
	// INFO: Needs PeerSet from dynamic_participants
	// peerSet, err := p.Store.GetPeerSet(roundReceived)
	// if err != nil {
	// 	return nil, err
	// }

	for _, peer := range p.Participants.ToPubKeySlice() {
		if _, ok := roots[peer]; !ok {
			var root Root
			lastConsensusEventHash, isRoot, err := p.Store.LastConsensusEventFrom(peer)
			if err != nil {
				return Frame{}, err
			}
			if isRoot {
				root, _ = p.Store.GetRoot(peer)
			} else {
				lastConsensusEvent, err := p.Store.GetEventBlock(lastConsensusEventHash)
				if err != nil {
					return Frame{}, err
				}
				root, err = p.createRoot(lastConsensusEvent)
				if err != nil {
					return Frame{}, err
				}
			}
			roots[peer] = root
		}
	}

	// Some Events in the Frame might have other-parents that are outside of the
	// Frame (cf root.go ex 2)
	// When inserting these Events in a newly reset poset, the CheckOtherParent
	// method would return an error because the other-parent would not be found.
	// So we make it possible to also look for other-parents in the creator's Root.
	treated := map[EventHash]bool{}
	eventMessages := make([]*EventMessage, len(events))
	for i, ev := range events {
		hash := ev.Hash()
		otherParent := ev.OtherParent()
		selfParent := ev.SelfParent()
		treated[ev.Hash()] = true
		if !otherParent.Zero() {
			opt, ok := treated[otherParent]
			if !opt || !ok {
				if !selfParent.Equal(roots[ev.GetCreator()].SelfParent.Hash) {
					other, err := p.createOtherParentRootEvent(ev)
					if err != nil {
						return Frame{}, err
					}
					roots[ev.GetCreator()].Others[hash.String()] = &other
				}
			}
		}
		eventMessages[i] = ev.Message
	}

	// order roots
	orderedRoots := make([]*Root, p.Participants.Len())
	for i, peer := range p.Participants.ToPeerSlice() {
		root := roots[peer.PubKeyHex]
		orderedRoots[i] = new(Root)
		*orderedRoots[i] = root
	}

	res := Frame{
		Round:     roundReceived,
		Roots:     orderedRoots,
		Events:    eventMessages,
		StateHash: stateHash.Bytes(),
	}

	if err := p.Store.SetFrame(res); err != nil {
		return Frame{}, err
	}

	return res, nil
}

// ApplyInternalTransactions calcs new PoS-state from prev round's state and returns its hash.
func (p *Poset) ApplyInternalTransactions(round int64, orderedEvents []Event) (res hash.Hash, err error) {
	// TODO: set RoundNIL = 0, condition change to "round <= RoundNIL"
	if round <= 0 {
		err = fmt.Errorf("empty round is not allowed")
		return
	}

	var prevState hash.Hash
	if round == 1 {
		prevState = p.Store.StateRoot()
	} else {
		var prevFrame Frame
		prevFrame, err = p.Store.GetFrame(round - 1)
		if err != nil {
			return
		}
		prevState = hash.FromBytes(prevFrame.StateHash)
	}

	statedb, err := state.New(prevState, p.Store.StateDB())
	if err != nil {
		return
	}

	for _, ev := range orderedEvents {
		creator, ok := p.Participants.ReadByID(ev.CreatorID())
		if !ok {
			p.logger.Warnf("Unknown participant ID=%d", ev.CreatorID())
			continue
		}
		sender := creator.Address()
		if body := ev.Message.GetBody(); body != nil {
			for _, tx := range body.GetInternalTransactions() {
				p.logger.Debug("ApplyInternalTransaction", tx)
				if statedb.FreeBalance(hash.Peer(sender)) < tx.Amount {
					p.logger.Warn("Balance is not enough", sender, tx.Amount)
					continue
				}

				reciver := hash.HexToPeer(tx.Receiver)
				statedb.Transfer(hash.Peer(sender), reciver, tx.Amount)
			}
		}
	}

	res, err = statedb.Commit(true)
	return
}

// ProcessSigPool runs through the SignaturePool and tries to map a Signature to
// a known Block. If a Signature is found to be valid for a known Block, it is
// appended to the block and removed from the SignaturePool
func (p *Poset) ProcessSigPool() error {
	processedSignatures := map[int64]bool{} // index in SigPool => Processed?
	defer p.removeProcessedSignatures(processedSignatures)

	for i, bs := range p.SigPool {
		// check if validator belongs to list of participants
		validatorHex := fmt.Sprintf("0x%X", bs.Validator)
		if _, ok := p.Participants.ReadByPubKey(validatorHex); !ok {
			p.logger.WithFields(logrus.Fields{
				"index":     bs.Index,
				"validator": validatorHex,
			}).Warning("Verifying Block signature. Unknown validator")
			continue
		}
		// only check if bs is greater than AnchorBlock, otherwise simply remove
		if p.AnchorBlock == nil ||
			bs.Index > *p.AnchorBlock {
			block, err := p.Store.GetBlock(bs.Index)
			if err != nil {
				p.logger.WithFields(logrus.Fields{
					"index": bs.Index,
					"msg":   err,
				}).Warning("Verifying Block signature. Could not fetch Block")
				continue
			}
			valid, err := block.Verify(bs)
			if err != nil {
				p.logger.WithFields(logrus.Fields{
					"index": bs.Index,
					"msg":   err,
				}).Error("Verifying Block signature")
				return err
			}
			if !valid {
				peer, ok := p.Participants.ReadByPubKey(validatorHex)
				p.logger.WithFields(logrus.Fields{
					"index":     bs.Index,
					"validator": peer,
					"ok":        ok,
					"block":     block,
				}).Warning("Verifying Block signature. Invalid signature")
				continue
			}

			if err := block.SetSignature(bs); err != nil {
				p.logger.Fatal(err)
			}

			if err := p.Store.SetBlock(block); err != nil {
				p.logger.WithFields(logrus.Fields{
					"index": bs.Index,
					"msg":   err,
				}).Warning("Saving Block")
			}

			if len(block.Signatures) > p.trustCount &&
				(p.AnchorBlock == nil ||
					block.Index() > *p.AnchorBlock) {
				p.setAnchorBlock(block.Index())
				p.logger.WithFields(logrus.Fields{
					"block_index": block.Index(),
					"signatures":  len(block.Signatures),
					"trustCount":  p.trustCount,
				}).Debug("Setting AnchorBlock")
			}
		}

		processedSignatures[int64(i)] = true
	}

	return nil
}

// GetAnchorBlockWithFrame returns the AnchorBlock and the corresponding Frame.
// This can be used as a base to Reset a Poset
func (p *Poset) GetAnchorBlockWithFrame() (Block, Frame, error) {

	if p.AnchorBlock == nil {
		return Block{}, Frame{}, fmt.Errorf("no Anchor Block")
	}

	block, err := p.Store.GetBlock(*p.AnchorBlock)
	if err != nil {
		return Block{}, Frame{}, err
	}

	frame, err := p.GetFrame(block.RoundReceived())
	if err != nil {
		return Block{}, Frame{}, err
	}

	return block, frame, nil
}

// Reset clears the Poset and resets it from a new base.
func (p *Poset) Reset(block Block, frame Frame) error {

	// Clear all state
	p.firstLastConsensusRoundLocker.Lock()
	p.LastConsensusRound = nil
	p.FirstConsensusRound = nil
	p.firstLastConsensusRoundLocker.Unlock()
	p.AnchorBlock = nil

	p.undeterminedEventsLocker.Lock()
	p.UndeterminedEvents = EventHashes{}
	p.undeterminedEventsLocker.Unlock()
	p.PendingRounds = []*pendingRound{}
	p.pendingLoadedEventsLocker.Lock()
	p.pendingLoadedEvents = 0
	p.pendingLoadedEventsLocker.Unlock()
	p.topologicalIndex = 0

	cacheSize := p.Store.CacheSize()
	dominatorCache, err := lru.New(cacheSize)
	if err != nil {
		p.logger.Fatal("Unable to reset Poset.dominatorCache")
	}
	selfDominatorCache, err := lru.New(cacheSize)
	if err != nil {
		p.logger.Fatal("Unable to reset Poset.selfDominatorCache")
	}
	strictlyDominatedCache, err := lru.New(cacheSize)
	if err != nil {
		p.logger.Fatal("Unable to reset Poset.strictlyDominatedCache")
	}
	roundCache, err := lru.New(cacheSize)
	if err != nil {
		p.logger.Fatal("Unable to reset Poset.roundCache")
	}
	p.dominatorCache = dominatorCache
	p.selfDominatorCache = selfDominatorCache
	p.strictlyDominatedCache = strictlyDominatedCache
	p.roundCache = roundCache

	participants := p.Participants.ToPeerSlice()

	// Initialize new Roots
	rootMap := map[string]Root{}
	for id, root := range frame.Roots {
		p := participants[id]
		rootMap[p.PubKeyHex] = *root
	}
	if err := p.Store.Reset(rootMap); err != nil {
		return err
	}

	// Insert Block
	if err := p.Store.SetBlock(block); err != nil {
		return err
	}

	p.setLastConsensusRound(block.RoundReceived())

	// Insert Frame Events
	for _, ev := range frame.Events {
		if err := p.InsertEvent(ev.ToEvent(), false); err != nil {
			return err
		}
	}

	return nil
}

// Bootstrap loads all Events from the Store's DB (if there is one) and feeds
// them to the Poset (in topological order) for consensus ordering. After this
// method call, the Poset should be in a state coherent with the 'tip' of the
// Poset
func (p *Poset) Bootstrap() error {
	// Retrieve the Events from the underlying DB. They come out in topological
	// order
	topologicalEvents, err := p.Store.TopologicalEvents()
	if err != nil {
		return err
	}

	// Insert the Events in the Poset
	for _, e := range topologicalEvents {
		if err := p.InsertEvent(e, true); err != nil {
			return err
		}
	}

	// Compute the consensus order of Events
	if err := p.DivideRounds(); err != nil {
		return err
	}
	if err := p.DecideAtropos(); err != nil {
		return err
	}
	if err := p.DecideRoundReceived(); err != nil {
		return err
	}
	if err := p.ProcessDecidedRounds(); err != nil {
		return err
	}
	if err := p.ProcessSigPool(); err != nil {
		return err
	}

	return nil
}

// ReadWireInfo converts a WireEvent to an Event by replacing int IDs with the
// corresponding public keys.
func (p *Poset) ReadWireInfo(wevent WireEvent) (*Event, error) {
	var (
		selfParent  EventHash = GenRootSelfParent(wevent.Body.CreatorID)
		otherParent EventHash
		err         error
	)
	if wevent.Body.OtherParentCreatorID != 0 {
		otherParent = GenRootSelfParent(wevent.Body.OtherParentCreatorID)
	}

	creator, ok := p.Participants.ReadByID(wevent.Body.CreatorID)
	// TODO FIXIT: creator can be nil when wevent.Body.CreatorID == 0
	if !ok {
		return nil, fmt.Errorf("unknown wevent.Body.CreatorID=%v", wevent.Body.CreatorID)
	}
	creatorBytes, err := hex.DecodeString(creator.PubKeyHex[2:])
	if err != nil {
		return nil, fmt.Errorf("hexDecodeString(creator.PubKeyHex[2:]): %v", err)
	}

	if wevent.Body.SelfParentIndex >= 0 {
		selfParent, err = p.Store.ParticipantEvent(creator.PubKeyHex, wevent.Body.SelfParentIndex)
		if err != nil {
			return nil, fmt.Errorf("p.Store.ParticipantEvent(creator.PubKeyHex %v, wevent.Body.SelfParentIndex %v): %v",
				creator.PubKeyHex, wevent.Body.SelfParentIndex, err)
		}
	}
	if wevent.Body.OtherParentIndex >= 0 {
		otherParentCreator, ok := p.Participants.ReadByID(wevent.Body.OtherParentCreatorID)
		if ok {
			otherParent, err = p.Store.ParticipantEvent(otherParentCreator.PubKeyHex, wevent.Body.OtherParentIndex)
			if err != nil {
				// PROBLEM Check if other parent can be found in the root
				// problem, we do not known the WireEvent's EventHash, and
				// we do not know the creators of the roots RootEvents
				root, err := p.Store.GetRoot(creator.PubKeyHex)
				if err != nil {
					return nil, fmt.Errorf("p.Store.GetRoot(creator.PubKeyHex %v): %v", creator.PubKeyHex, err)
				}
				// loop through others
				found := false
				for _, re := range root.Others {
					if re.CreatorID == wevent.Body.OtherParentCreatorID &&
						re.Index == wevent.Body.OtherParentIndex {
						otherParent.Set(re.Hash)
						found = true
						break
					}
				}

				if !found {
					return nil, fmt.Errorf("OtherParent not found")
				}
			}
		} else {
			// unknown participant
			// TODO: we should handle this nicely
			return nil, errors.New("unknown participant")
		}
	}

	if len(wevent.FlagTable) == 0 {
		return nil, fmt.Errorf("flag table is null")
	}

	signatureValues := wevent.BlockSignatures(creatorBytes)
	blockSignatures := make([]*BlockSignature, len(signatureValues))
	for i, v := range signatureValues {
		blockSignatures[i] = new(BlockSignature)
		*blockSignatures[i] = v
	}
	body := EventBody{
		Transactions:         wevent.Body.Transactions,
		InternalTransactions: inter.InternalTransactionsToWire(wevent.Body.InternalTransactions),
		Parents:              [][]byte{selfParent.Bytes(), otherParent.Bytes()},
		Creator:              creatorBytes,
		Index:                wevent.Body.Index,
		BlockSignatures:      blockSignatures,
	}

	event := &Event{
		Message: &EventMessage{
			Body:                 &body,
			Signature:            wevent.Signature,
			FlagTable:            wevent.FlagTable,
			ClothoProof:          wevent.ClothoProof,
			SelfParentIndex:      wevent.Body.SelfParentIndex,
			OtherParentCreatorID: wevent.Body.OtherParentCreatorID,
			OtherParentIndex:     wevent.Body.OtherParentIndex,
			CreatorID:            wevent.Body.CreatorID,
		},
		roundReceived:    RoundNIL,
		round:            RoundNIL,
		lamportTimestamp: LamportTimestampNIL,
	}

	p.logger.WithFields(logrus.Fields{
		"event.Signature":  event.Message.Signature,
		"wevent.Signature": wevent.Signature,
	}).Debug("Return Event from ReadFromWire")

	return event, nil
}

// CheckBlock returns an error if the Block does not contain valid signatures
// from MORE than 1/3 of participants
func (p *Poset) CheckBlock(block Block) error {
	validSignatures := 0
	for _, s := range block.GetBlockSignatures() {
		ok, _ := block.Verify(s)
		if ok {
			validSignatures++
		}
	}
	if validSignatures <= p.trustCount {
		return fmt.Errorf("not enough valid signatures: got %d, need %d", validSignatures, p.trustCount+1)
	}

	p.logger.WithField("valid_signatures", validSignatures).Debug("CheckBlock")
	return nil
}

/*******************************************************************************
Setters
*******************************************************************************/

func (p *Poset) setLastConsensusRound(i int64) {
	p.firstLastConsensusRoundLocker.Lock()
	defer p.firstLastConsensusRoundLocker.Unlock()
	if p.LastConsensusRound == nil {
		p.LastConsensusRound = new(int64)
	}
	*p.LastConsensusRound = i

	if p.FirstConsensusRound == nil {
		p.FirstConsensusRound = new(int64)
		*p.FirstConsensusRound = i
	}
}

func (p *Poset) setAnchorBlock(i int64) {
	if p.AnchorBlock == nil {
		p.AnchorBlock = new(int64)
	}
	*p.AnchorBlock = i
}

/*
 * Getters
 */

// GetPeerFlagTableOfRandomUndeterminedEvent returns the flag table for undermined events
func (p *Poset) GetPeerFlagTableOfRandomUndeterminedEvent() (map[string]int64, error) {
	p.undeterminedEventsLocker.RLock()
	defer p.undeterminedEventsLocker.RUnlock()

	perm := rand.Perm(len(p.UndeterminedEvents))
	for i := 0; i < len(perm); i++ {
		hash := p.UndeterminedEvents[perm[i]]
		ev, err := p.Store.GetEventBlock(hash)
		if err != nil {
			continue
		}
		ft, err := ev.GetFlagTable()
		if err != nil {
			continue
		}
		if len(ft) >= len(p.Participants.Sorted) {
			continue
		}
		tablePeers := make(map[string]int64, len(ft))
		for e := range ft {
			ex, err := p.Store.GetEventBlock(e)
			if err == nil {
				tablePeers[ex.GetCreator()] = 1
			}
		}
		return tablePeers, nil
	}
	return nil, nil
}

// GetUndeterminedEvents returns all the undetermined events
func (p *Poset) GetUndeterminedEvents() EventHashes {
	p.undeterminedEventsLocker.RLock()
	defer p.undeterminedEventsLocker.RUnlock()
	return p.UndeterminedEvents
}

// GetPendingLoadedEvents returns all the pending events
func (p *Poset) GetPendingLoadedEvents() int64 {
	p.pendingLoadedEventsLocker.RLock()
	defer p.pendingLoadedEventsLocker.RUnlock()
	return p.pendingLoadedEvents
}

// GetLastConsensusRound returns the last consensus round
func (p *Poset) GetLastConsensusRound() int64 {
	p.firstLastConsensusRoundLocker.RLock()
	defer p.firstLastConsensusRoundLocker.RUnlock()
	if p.LastConsensusRound == nil {
		// -2 is less that undefined round index, -1
		return -2
	}
	return *p.LastConsensusRound
}

// GetConsensusTransactionsCount returns the count of finalized transactions
func (p *Poset) GetConsensusTransactionsCount() uint64 {
	p.consensusTransactionsLocker.RLock()
	defer p.consensusTransactionsLocker.RUnlock()
	return p.ConsensusTransactions
}

/*******************************************************************************
   Helpers
*******************************************************************************/

func randomShift(hash EventHash) bool {
	if !hash.Zero() && hash[len(hash)/2] == 0 {
		return false
	}
	return true
}
