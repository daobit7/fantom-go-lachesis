package posposet

import (
	"github.com/Fantom-foundation/go-lachesis/src/hash"
	"github.com/Fantom-foundation/go-lachesis/src/posposet/wire"
)

// Block is a chain block.
type Block struct {
	Index  uint64
	Events hash.EventsSlice
}

// ToWire converts to proto.Message.
func (e *Block) ToWire() *wire.Block {
	return &wire.Block{
		Index:  e.Index,
		Events: e.Events.ToWire(),
	}
}

// WireToBlock converts from wire.
func WireToBlock(w *wire.Block) *Block {
	if w == nil {
		return nil
	}
	return &Block{
		Index:  w.Index,
		Events: hash.WireToEventHashSlice(w.Events),
	}
}

// NewBlock makes main chain block from topological ordered events.
func NewBlock(index uint64, ordered Events) *Block {
	events := make(hash.EventsSlice, len(ordered))
	for i, e := range ordered {
		events[i] = e.Hash()
	}

	return &Block{
		Index:  index,
		Events: events,
	}
}
