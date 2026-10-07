package refreshqueue

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"orchids-api/internal/store"
)

type credentialGate struct {
	token chan struct{}
	users int
}

var credentialGates = struct {
	sync.Mutex
	entries map[string]*credentialGate
}{entries: make(map[string]*credentialGate)}

// AcquireCredential serializes rotating grants across client instances sharing
// an account store. Waiters are cancelable and idle gates are removed.
func AcquireCredential(ctx context.Context, provider string, owner any, id int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == 0 || nilCredentialOwner(owner) {
		return func() {}, ctx.Err()
	}
	key := fmt.Sprintf("%s:%T:%p:%d", provider, owner, owner, id)
	credentialGates.Lock()
	g := credentialGates.entries[key]
	if g == nil {
		g = &credentialGate{token: make(chan struct{}, 1)}
		credentialGates.entries[key] = g
	}
	g.users++
	credentialGates.Unlock()
	drop := func() {
		credentialGates.Lock()
		defer credentialGates.Unlock()
		g.users--
		if g.users == 0 {
			delete(credentialGates.entries, key)
		}
	}
	select {
	case g.token <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-g.token; drop() }) }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// LatestAccount is optional for legacy test stores; production readers are
// authoritative and errors must stop a grant using possibly consumed material.
func LatestAccount(ctx context.Context, owner any, id int64) (*store.Account, error) {
	if id == 0 || nilCredentialOwner(owner) {
		return nil, nil
	}
	if reader, ok := owner.(interface {
		GetAccount(context.Context, int64) (*store.Account, error)
	}); ok {
		acc, err := reader.GetAccount(ctx, id)
		if err != nil {
			return nil, err
		}
		if acc == nil {
			return nil, fmt.Errorf("credential account no longer exists")
		}
		return acc, nil
	}
	return nil, nil
}

func nilCredentialOwner(owner any) bool {
	return owner == nil || (reflect.ValueOf(owner).Kind() == reflect.Pointer && reflect.ValueOf(owner).IsNil())
}
