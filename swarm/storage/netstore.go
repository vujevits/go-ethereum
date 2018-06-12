// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package storage

import (
	"context"
	"encoding/hex"
	"sync"
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru"
)

type (
	FetchFunc    func(ctx context.Context)
	NewFetchFunc func(ctx context.Context, offer Address, peers *sync.Map) FetchFunc
)

// NetStore is an extention of local storage
// it implements the ChunkStore interface
// on request it initiates remote cloud retrieval using a fetcher
// fetchers are unique to a chunk and are stored in fetchers LRU memory cache
// fetchFuncFactory is a factory object to create a fetch function for a specific chunk address
type NetStore struct {
	mu           sync.Mutex
	store        ChunkStore
	fetchers     *lru.Cache
	newFetchFunc NewFetchFunc
}

// NewNetStore creates a new NetStore object using the given local store. newFetchFunc is a
// constructor function that can create a fetch function for a specific chunk address.
func NewNetStore(store ChunkStore, newFetchFunc NewFetchFunc) (*NetStore, error) {
	fetchers, err := lru.New(defaultChunkRequestsCacheCapacity)
	if err != nil {
		return nil, err
	}
	return &NetStore{
		store:        store,
		fetchers:     fetchers,
		newFetchFunc: newFetchFunc,
	}, nil
}

// Put stores a chunk in localstore, returns a wait function to wait for
// storage unless it is found
func (n *NetStore) Put(ch Chunk) (func(ctx context.Context) error, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	wait, err := n.store.Put(ch)
	if err != nil {
		return nil, err
	}
	// if chunk was already in store (wait f is nil)
	if wait == nil {
		return nil, nil
	}
	// if chunk is now put in store, check if there was an active fetcher
	f, _ := n.fetchers.Get(ch.Address())
	// if there is, deliver the chunk to requestors via fetcher
	if f != nil {
		f.(*fetcher).deliver(ch)
	}
	return wait, nil
}

// Get retrieves the chunk from the NetStore DPA synchronously
// it calls NetStore.get. If the chunk is not in local Storage
// it calls fetch with the request, which blocks until the chunk
// arrived or context is done
func (n *NetStore) Get(rctx context.Context, ref Address) (Chunk, error) {
	chunk, fetch, err := n.get(rctx, ref)
	if fetch == nil {
		return chunk, err
	}
	return fetch(rctx)
}

// Has
func (n *NetStore) Has(ctx context.Context, ref Address) func(context.Context) (Chunk, error) {
	_, fetch, _ := n.get(ctx, ref)
	return fetch
}

// Close chunk store
func (n *NetStore) Close() {
	n.store.Close()
}

// get attempts at retrieving the chunk from LocalStore
// if it is not found, attempts at retrieving an existing fetchers
// if none exists, creates one and saves it in the fetchers cache
// From here on, all Get will hit on this fetcher until the chunk is delivered
// or all fetcher contexts are done
// it returns a chunk, a fetcher function and an error
// if chunk is nil, fetcher needs to be called with a context to return the chunk
func (n *NetStore) get(ctx context.Context, ref Address) (Chunk, func(context.Context) (Chunk, error), error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	chunk, err := n.store.Get(ctx, ref)
	if err == nil {
		return chunk, nil, nil
	}
	f := n.getOrCreateFetcher(ref)
	return nil, f.Fetch, nil
}

// getOrCreateFetcher attempts at retrieving an existing fetchers
// if none exists, creates one and saves it in the fetchers cache
// caller must hold the lock
func (n *NetStore) getOrCreateFetcher(ref Address) *fetcher {
	key := hex.EncodeToString(ref)
	f, ok := n.fetchers.Get(key)
	if ok {
		return f.(*fetcher)
	}
	// create the context during which fetching is kept alive
	ctx, cancel := context.WithCancel(context.Background())
	// destroy is called when all requests finish
	destroy := func() {
		// remove fetcher from fetchers
		n.fetchers.Remove(key)
		// stop fetcher by cancelling context called when
		// all requests cancelled/timedout or chunk is delivered
		cancel()
	}
	peers := &sync.Map{}
	fetcher := newFetcher(ref, n.newFetchFunc(ctx, ref, peers), destroy, peers)
	n.fetchers.Add(key, fetcher)

	return fetcher
}

type fetcher struct {
	addr       Address       // adress of chunk
	chunk      Chunk         // fetcher can set the chunk on the fetcher
	deliveredC chan struct{} // chan signalling chunk delivery to requests
	fetch      FetchFunc     // remote fetch function to be called with a request source taken from the context
	cancel     func()        // cleanup function for the remote fetcher to call when all upstream contexts are called
	peers      *sync.Map     // the peers which asked for the chunk
	requestCnt int32         // number of requests on this chunk. If all the requests are done (delivered or context is done) the cancel function is called
}

func newFetcher(addr Address, fetch FetchFunc, cancel func(), peers *sync.Map) *fetcher {
	return &fetcher{
		addr:       addr,
		deliveredC: make(chan struct{}),
		fetch:      fetch,
		cancel:     cancel,
		peers:      peers,
	}
}

// Fetch fetches the chunk synchronously, it is called by NetStore.Get is the chunk is not available
// locally.
func (f *fetcher) Fetch(rctx context.Context) (Chunk, error) {
	atomic.AddInt32(&f.requestCnt, 1)
	defer func() {
		// if all the requests are done the fetcher can be cancelled
		if atomic.AddInt32(&f.requestCnt, -1) == 0 {
			f.cancel()
		}
	}()

	// The peer asking for the chunk. Maybe this should be a function parameter?
	peer := rctx.Value("peer")
	if peer != nil {
		f.peers.Store(peer, true)
		defer f.peers.Delete(peer)
	}

	f.fetch(rctx)

	// wait until either the chunk is delivered or the context is done
	select {
	case <-rctx.Done():
		return nil, rctx.Err()
	case <-f.deliveredC:
		return f.chunk, nil
	}
}

// deliver is called by NetStore.Put to notify all pending
// requests
func (f *fetcher) deliver(ch Chunk) {
	f.chunk = ch
	close(f.deliveredC)
}
