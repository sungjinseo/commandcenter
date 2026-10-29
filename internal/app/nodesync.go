package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/repo"
)

// A barrier across the nodes of a PostgreSQL database (repo.Store.Sync):
// the asking node sends "storesync:<id>" until every other live node
// answered "storesynced:<id> <node>", which each sends once it applied the
// stored blob stores, groups and moves and finished the blob writes it
// began before a move (answerSync).

// syncResend is how often syncNodes asks again the nodes that have not
// answered: a request or an answer sent while a listener reconnected is
// lost. syncLogEvery is how often it names them in the log. Vars for tests.
var (
	syncResend   = 2 * time.Second
	syncLogEvery = 30 * time.Second
)

// syncNodes is repo.Store.SyncNodes: this node's WaitWrites, then the
// answers of every other live node (meta.DB.Nodes). A node that does not
// answer is waited for until it does, its heartbeat expires (a crash) or
// ctx ends.
func (a *App) syncNodes(ctx context.Context) error {
	if err := a.Store.WaitWrites(ctx); err != nil {
		return err
	}
	a.syncAnswers.Do(func() {
		a.DB.Subscribe("storesynced", func(arg string) {
			id, node, _ := strings.Cut(arg, " ")
			a.syncMu.Lock()
			ch := a.syncWait[id]
			a.syncMu.Unlock()
			if ch != nil {
				select {
				case ch <- node:
				default: // full: the next ask brings it again
				}
			}
		})
	})
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	answers := make(chan string, 64)
	a.syncMu.Lock()
	if a.syncWait == nil {
		a.syncWait = map[string]chan string{}
	}
	a.syncWait[id] = answers
	a.syncMu.Unlock()
	defer func() {
		a.syncMu.Lock()
		delete(a.syncWait, id)
		a.syncMu.Unlock()
	}()
	answered := map[string]bool{a.Node: true}
	logged := time.Now()
	for {
		if err := a.DB.Notify(ctx, "storesync:"+id); err != nil {
			return fmt.Errorf("ask the other nodes to apply the blob store change: %w", err)
		}
		resend := time.After(syncResend)
	wait:
		for {
			nodes, err := a.DB.Nodes(ctx)
			if err != nil {
				return err
			}
			var missing []string
			for _, n := range nodes {
				if n.Alive && !answered[n.ID] {
					missing = append(missing, n.ID)
				}
			}
			if len(missing) == 0 {
				return nil
			}
			if time.Since(logged) >= syncLogEvery {
				a.log.Warn("waiting for nodes to apply a blob store change; a node that keeps running without answering holds it up",
					"nodes", strings.Join(missing, ", "))
				logged = time.Now()
			}
			select {
			case n := <-answers:
				answered[n] = true
			case <-resend:
				break wait
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// answerSync answers syncNodes's request id (the "storesync" topic) once
// this node applied the stored blob stores, groups and moves, under the
// registry lock as a write does, and finished the blob writes it began
// before a move. A node that cannot open a store or group as stored does
// not answer: the asker waits. Its own requests it ignores.
func (a *App) answerSync(id string) {
	a.syncMu.Lock()
	own := a.syncWait[id] != nil
	a.syncMu.Unlock()
	if own {
		return
	}
	if _, busy := a.syncing.LoadOrStore(id, true); busy {
		return
	}
	go func() {
		defer a.syncing.Delete(id)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		err := a.Registry.WithWriteLock(ctx, func(*repo.Snapshot) error { // its refresh syncs the moves
			return a.Store.SyncStores(ctx, a.Config.DataDir)
		})
		if err == nil {
			err = a.Store.WaitWrites(ctx)
		}
		if err == nil {
			err = a.DB.Notify(ctx, "storesynced:"+id+" "+a.Node)
		}
		if err != nil {
			a.log.Warn("could not apply a blob store change another node waits for; it waits until this node can", "err", logsafe.Err(err))
		}
	}()
}
