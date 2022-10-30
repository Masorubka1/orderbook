package orderbook

import (
	"runtime"
	"sync/atomic"

	"golang.org/x/sys/cpu"
)

// template type ItemPool(PoolItem)
type PoolItem struct{}

// A simple order pool.
type ItemPool struct {
	ch *itemChan
}

func NewItemPool(maxSize uint64) *ItemPool {
	ch := newItemChan(maxSize)
	p := &ItemPool{
		ch: ch,
	}

	for !ch.IsFull() {
		ch.Put(&PoolItem{})
	}

	return p
}

func (p *ItemPool) Get() *PoolItem {
	if p.ch.IsEmpty() {
		return &PoolItem{}
	}

	return p.ch.Read()
}

func (p *ItemPool) Put(o *PoolItem) {
	if o == nil {
		return
	}

	if p.ch.IsFull() {
		// Leave it to GC
		return
	}

	p.ch.Put(o)
}

// CONTENT BELOW GENERATED USING
// go:generate gotemplate "github.com/geseq/fastchan" itemChan(*PoolItem)
// --------------------------------------------------------------------
//
//
// Copyright 2012 Darren Elwood <darren@textnode.com> http://www.textnode.com @textnode
// Copyright 2021 E Sequeira
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// A fast chan replacement based on textnode/gringo
//
// N.B. To see the performance benefits of gringo versus Go's channels, you must have multiple goroutines
// and GOMAXPROCS > 1.

// Known Limitations:
//
// *) At most (2^64)-2 items can be written to the queue.
// *) The size of the queue must be a power of 2.
//
// Suggestions:
//
// *) If you have enough cores you can change from runtime.Gosched() to a busy loop.
//

// FastChan is a minimalist queue replacement for channels with higher throughput
type itemChan struct {
	// The padding members 1 to 5 below are here to ensure each item is on a separate cache line.
	// This prevents false sharing and hence improves performance.
	_                  cpu.CacheLinePad
	indexMask          uint64
	_                  cpu.CacheLinePad
	lastCommittedIndex uint64
	_                  cpu.CacheLinePad
	nextFreeIndex      uint64
	_                  cpu.CacheLinePad
	readerIndex        uint64
	_                  cpu.CacheLinePad
	contents           []*PoolItem
	_                  cpu.CacheLinePad
}

// NewFastChan creates a new channel
func newItemChan(size uint64) *itemChan {
	size = roundUpNextPowerOfTwoItemChan(size)
	return &itemChan{
		lastCommittedIndex: 0,
		nextFreeIndex:      0,
		readerIndex:        0,
		indexMask:          size - 1,
		contents:           make([]*PoolItem, size),
	}
}

// Put writes a CacheItem to the front of the channel
func (c *itemChan) Put(value *PoolItem) {
	//Wait for reader to catch up, so we don't clobber a slot which it is (or will be) reading
	for atomic.LoadUint64(&c.nextFreeIndex)+1 > (atomic.LoadUint64(&c.readerIndex) + c.indexMask) {
		runtime.Gosched()
	}

	var myIndex = atomic.AddUint64(&c.nextFreeIndex, 1)
	//Write the item into it's slot
	c.contents[myIndex&c.indexMask] = value

	//Increment the lastCommittedIndex so the item is available for reading
	for !atomic.CompareAndSwapUint64(&c.lastCommittedIndex, myIndex-1, myIndex) {
		runtime.Gosched()
	}
}

// Read reads and removes a CacheItem from the back of the channel
func (c *itemChan) Read() *PoolItem {
	//If reader has out-run writer, wait for a value to be committed
	for atomic.LoadUint64(&c.readerIndex)+1 > atomic.LoadUint64(&c.lastCommittedIndex) {
		runtime.Gosched()
	}

	var myIndex = atomic.AddUint64(&c.readerIndex, 1)
	return c.contents[myIndex&c.indexMask]
}

// Empty the channel
func (c *itemChan) Empty() {
	c.lastCommittedIndex = 0
	c.nextFreeIndex = 0
	c.readerIndex = 0
}

// Size gets the size of the contents in the channel buffer
func (c *itemChan) Size() uint64 {
	return atomic.LoadUint64(&c.lastCommittedIndex) - atomic.LoadUint64(&c.readerIndex)
}

// IsEmpty checks if the channel is empty
func (c *itemChan) IsEmpty() bool {
	return atomic.LoadUint64(&c.readerIndex) >= atomic.LoadUint64(&c.lastCommittedIndex)
}

// IsFull checks if the channel is full
func (c *itemChan) IsFull() bool {
	return atomic.LoadUint64(&c.nextFreeIndex) >= (atomic.LoadUint64(&c.readerIndex) + c.indexMask)
}

func roundUpNextPowerOfTwoItemChan(v uint64) uint64 {
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	v |= v >> 32
	v++
	return v
}
