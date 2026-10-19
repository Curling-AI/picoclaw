package memory

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"testing"
	"time"
)

// keyInShard returns the first "<prefix>-<n>" key that hashes to shard.
func keyInShard(t *testing.T, prefix string, shard uint32) string {
	t.Helper()
	for i := 0; i < 1_000_000; i++ {
		key := fmt.Sprintf("%s-%d", prefix, i)
		h := fnv.New32a()
		h.Write([]byte(key))
		if h.Sum32()%numLockShards == shard {
			return key
		}
	}
	t.Fatalf("no %q key found for shard %d", prefix, shard)
	return ""
}

// Two pairs land on the same two shards in opposite key order: the key of
// one pair shares a shard with the alias of the other. Ordering the locks by
// key made each promotion hold one shard while waiting for the other one, as
// two cron turns that started in the same second did in production.
func TestPromoteAliasHistory_CrossedShardsDoNotDeadlock(t *testing.T) {
	store := newTestStore(t)
	const shardX, shardY = 15, 26
	firstKey, firstAlias := keyInShard(t, "a", shardX), keyInShard(t, "b", shardY)
	secondKey, secondAlias := keyInShard(t, "a", shardY), keyInShard(t, "b", shardX)
	if firstKey > firstAlias || secondKey > secondAlias {
		t.Fatalf("each key must sort before its alias: %q/%q, %q/%q", firstKey, firstAlias, secondKey, secondAlias)
	}

	const iterations = 2000
	promote := func(key, alias string, wg *sync.WaitGroup) {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := store.PromoteAliasHistory(context.Background(), key, nil, []string{alias}); err != nil {
				t.Errorf("PromoteAliasHistory(%q, %q): %v", key, alias, err)
				return
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go promote(firstKey, firstAlias, &wg)
	go promote(secondKey, secondAlias, &wg)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent promotions over crossed shards deadlocked")
	}
}

func TestLockSessionPair_LocksLowerShardFirst(t *testing.T) {
	store := newTestStore(t)
	const lowShard, highShard = 3, 40
	// The key sorts before the alias but lives in the higher shard: key order
	// and shard order disagree.
	key, alias := keyInShard(t, "a", highShard), keyInShard(t, "b", lowShard)

	store.locks[lowShard].Lock()
	acquired := make(chan func())
	go func() { acquired <- store.lockSessionPair(key, alias) }()

	// While the lower shard is taken, the pair must not hold the higher one.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !store.locks[highShard].TryLock() {
			store.locks[lowShard].Unlock()
			t.Fatal("lockSessionPair took the higher shard while waiting for the lower one")
		}
		store.locks[highShard].Unlock()
		time.Sleep(time.Millisecond)
	}

	store.locks[lowShard].Unlock()
	select {
	case unlock := <-acquired:
		unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("lockSessionPair did not acquire the pair after the lower shard was released")
	}
	for _, shard := range []int{lowShard, highShard} {
		if !store.locks[shard].TryLock() {
			t.Fatalf("shard %d still locked after unlock", shard)
		}
		store.locks[shard].Unlock()
	}
}

func TestLockSessionPair_SameShardLocksOnce(t *testing.T) {
	store := newTestStore(t)
	key, alias := keyInShard(t, "a", 7), keyInShard(t, "b", 7)

	unlock := store.lockSessionPair(key, alias)
	if store.locks[7].TryLock() {
		t.Fatal("shard not held by the pair")
	}
	unlock()
	if !store.locks[7].TryLock() {
		t.Fatal("shard still locked after unlock")
	}
	store.locks[7].Unlock()
}
