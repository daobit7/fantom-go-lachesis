package posnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Fantom-foundation/go-lachesis/src/posnode/api"
)

func TestGetPeerInfo(t *testing.T) {
	store := NewMemStore()
	n := NewForTests("server.fake", store, nil)
	n.StartService()
	defer n.StopService()

	c := NewForTests("client.fake", nil, nil)
	c.initClient()

	client, free, fail, err := c.ConnectTo(n.AsPeer())
	if !assert.NoError(t, err) {
		return
	}
	defer free()

	t.Run("existing peer", func(t *testing.T) {
		assert := assert.New(t)

		peer := FakePeer("unreachable")
		store.SetPeer(peer)

		ctx, cancel := context.WithTimeout(context.Background(), n.conf.ClientTimeout)
		defer cancel()

		got, err := client.GetPeerInfo(ctx, &api.PeerRequest{
			PeerID: peer.ID.Hex(),
		})
		if !assert.NoError(err) {
			fail(err)
			return
		}

		assert.Equal(peer, WireToPeer(got))
	})

	t.Run("no existing peer", func(t *testing.T) {
		assert := assert.New(t)

		ctx, cancel := context.WithTimeout(context.Background(), n.conf.ClientTimeout)
		defer cancel()

		resp, err := client.GetPeerInfo(ctx, &api.PeerRequest{
			PeerID: "unknown",
		})

		if !assert.Nil(resp) {
			return
		}

		if !assert.Error(err) {
			return
		} else {
			fail(err)
		}

		s := status.Convert(err)
		if !assert.NotNil(s) {
			return
		}
		assert.Equal(codes.NotFound, s.Code())
	})

}
