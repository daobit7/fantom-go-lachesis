package proxy

import (
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"github.com/Fantom-foundation/go-lachesis/src/common"
	"github.com/Fantom-foundation/go-lachesis/src/poset"
)

func TestInmemAppCalls(t *testing.T) {
	logger := common.NewTestLogger(t)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	handler := NewMockApp(ctrl)

	s := NewInmemAppProxy(handler, logger)
	defer s.Close()

	t.Run("#2 Receive block", func(t *testing.T) {
		assert := assert.New(t)
		block := poset.Block{
			Body: &poset.BlockBody{},
		}
		gold := []byte("123456")

		handler.EXPECT().
			CommitHandler(block).
			Return(gold, nil)

		answer, err := s.CommitBlock(block)
		if assert.NoError(err) {
			assert.Equal(gold, answer)
		}
	})

	t.Run("#3 Receive snapshot query", func(t *testing.T) {
		assert := assert.New(t)
		index := int64(1)
		gold := []byte("123456")

		handler.EXPECT().
			SnapshotHandler(index).
			Return(gold, nil)

		answer, err := s.GetSnapshot(index)
		if assert.NoError(err) {
			assert.Equal(gold, answer)
		}
	})

	t.Run("#4 Receive restore command", func(t *testing.T) {
		assert := assert.New(t)
		gold := []byte("123456")

		handler.EXPECT().
			RestoreHandler(gold).
			Return([]byte("state_hash"), nil)

		err := s.Restore(gold)
		assert.NoError(err)
	})
}
