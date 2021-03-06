package posnode

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Fantom-foundation/go-lachesis/src/hash"
	"github.com/Fantom-foundation/go-lachesis/src/inter/wire"
	"github.com/Fantom-foundation/go-lachesis/src/network"
	"github.com/Fantom-foundation/go-lachesis/src/posnode/api"
)

type service struct {
	listen network.ListenFunc
	server *grpc.Server
}

// StartService starts node service.
func (n *Node) StartService() {
	if n.server != nil {
		return
	}

	if n.service.listen == nil {
		n.service.listen = network.TCPListener
	}

	var genesis hash.Hash
	if n.consensus != nil {
		genesis = n.consensus.GetGenesisHash()
	}

	bind := n.NetAddrOf(n.host)
	n.server, _ = api.StartService(bind, n.key, genesis, n, n.Infof, n.service.listen)

	n.Info("service started")
}

// StopService stops node service.
func (n *Node) StopService() {
	if n.server == nil {
		return
	}
	n.server.GracefulStop()
	n.server = nil

	n.Info("service stopped")
}

/*
 * api.NodeServer implementation:
 */

// SyncEvents returns their known event heights excluding heights from request.
func (n *Node) SyncEvents(ctx context.Context, req *api.KnownEvents) (*api.KnownEvents, error) {
	if err := checkSource(ctx); err != nil {
		return nil, err
	}

	// food for discovery
	host := api.GrpcPeerHost(ctx)
	n.CheckPeerIsKnown(host, nil)
	for hex := range req.Lasts {
		peer := hash.HexToPeer(hex)
		n.CheckPeerIsKnown(host, &peer)
	}

	// response
	knowns := n.knownEvents()
	knownLasts := make(map[string]uint64, len(knowns))
	for id, h := range knowns {
		knownLasts[id.Hex()] = h
	}
	// TODO: should we remember other node's knowns for future request?
	// to_download := PeersHeightsDiff(req.Lasts, known)
	diff := PeersHeightsDiff(knownLasts, req.Lasts)
	return &api.KnownEvents{Lasts: diff}, nil
}

// GetEvent returns requested event.
func (n *Node) GetEvent(ctx context.Context, req *api.EventRequest) (*wire.Event, error) {
	if err := checkSource(ctx); err != nil {
		return nil, err
	}

	// food for discovery
	host := api.GrpcPeerHost(ctx)
	n.CheckPeerIsKnown(host, nil)

	var eventHash hash.Event

	if req.Hash != nil {
		eventHash.SetBytes(req.Hash)
	} else {
		creator := hash.HexToPeer(req.PeerID)
		h := n.store.GetEventHash(creator, req.Index)
		if h == nil {
			return nil, status.Error(codes.NotFound, fmt.Sprintf("event not found: %s-%d", req.PeerID, req.Index))
		}
		eventHash = *h
	}

	event := n.store.GetWireEvent(eventHash)
	if event == nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("event not found: %s", eventHash.Hex()))
	}

	return event, nil
}

// GetPeerInfo returns requested peer info.
func (n *Node) GetPeerInfo(ctx context.Context, req *api.PeerRequest) (*api.PeerInfo, error) {
	if err := checkSource(ctx); err != nil {
		return nil, err
	}

	// food for discovery
	host := api.GrpcPeerHost(ctx)
	n.CheckPeerIsKnown(host, nil)

	var id hash.Peer

	if req.PeerID == "" {
		id = n.ID
	} else {
		id = hash.HexToPeer(req.PeerID)
	}

	if id == n.ID { // self
		info := n.AsPeer()
		return info.ToWire(), nil
	}

	info := n.store.GetWirePeer(id)
	if info == nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("peer not found: %s", req.PeerID))
	}

	return info, nil
}

/*
 * Utils:
 */

// PeersHeightsDiff returns all heights excluding excepts.
func PeersHeightsDiff(all, excepts map[string]uint64) (res map[string]uint64) {
	res = make(map[string]uint64, len(all))
	for id, h0 := range all {
		if h1, ok := excepts[id]; !ok || h1 < h0 {
			res[id] = h0
		}
	}
	return
}

func checkSource(ctx context.Context) error {
	source := api.GrpcPeerID(ctx)
	if source.IsEmpty() {
		return status.Error(codes.Unauthenticated, "unknown peer")
	}
	return nil
}
