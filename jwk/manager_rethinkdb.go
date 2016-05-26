package jwk

import (
	"sync"

	"encoding/json"
	r "github.com/dancannon/gorethink"
	"github.com/go-errors/errors"
	"github.com/ory-am/hydra/pkg"
	"github.com/square/go-jose"
	"golang.org/x/net/context"
)

type RethinkManager struct {
	Session *r.Session
	Table   r.Term
	sync.RWMutex

	Keys    map[string]jose.JsonWebKeySet
}

func (m *RethinkManager) SetUpIndex() error {
	if _, err := m.Table.IndexWait("kid").Run(m.Session); err != nil {
		return errors.New(err)
	}
	return nil
}

func (m *RethinkManager) AddKey(set string, key *jose.JsonWebKey) error {
	if err := m.publishAdd(set, []jose.JsonWebKey{*key}); err != nil {
		return err
	}
	return nil
}

func (m *RethinkManager) AddKeySet(set string, keys *jose.JsonWebKeySet) error {
	if err := m.publishAdd(set, keys.Keys); err != nil {
		return err
	}
	return nil
}

func (m *RethinkManager) GetKey(set, kid string) (*jose.JsonWebKeySet, error) {
	m.Lock()
	defer m.Unlock()

	m.alloc()
	keys, found := m.Keys[set]
	if !found {
		return nil, errors.New(pkg.ErrNotFound)
	}

	result := keys.Key(kid)
	if len(result) == 0 {
		return nil, errors.New(pkg.ErrNotFound)
	}

	return &jose.JsonWebKeySet{
		Keys: result,
	}, nil
}

func (m *RethinkManager) GetKeySet(set string) (*jose.JsonWebKeySet, error) {
	m.Lock()
	defer m.Unlock()

	m.alloc()
	keys, found := m.Keys[set]
	if !found {
		return nil, errors.New(pkg.ErrNotFound)
	}

	if len(keys.Keys) == 0 {
		return nil, errors.New(pkg.ErrNotFound)
	}

	return &keys, nil
}

func (m *RethinkManager) DeleteKey(set, kid string) error {
	keys, err := m.GetKey(set, kid)
	if err != nil {
		return errors.New(err)
	}

	if err := m.publishDelete(set, keys.Keys); err != nil {
		return errors.New(err)
	}
	return nil
}

func (m *RethinkManager) DeleteKeySet(set string) error {
	if err := m.publishDeleteAll(set); err != nil {
		return errors.New(err)
	}
	return nil
}

func (m *RethinkManager) alloc() {
	if m.Keys == nil {
		m.Keys = make(map[string]jose.JsonWebKeySet)
	}
}

type rethinkSchema struct {
	KID string `gorethink:"kid"`
	Set string `gorethink:"set"`
	Key json.RawMessage `gorethink:"key"`
}

func (m *RethinkManager) publishAdd(set string, keys []jose.JsonWebKey) error {
	raws := make([]json.RawMessage, len(keys))
	for k, key := range keys {
		out, err := json.Marshal(key)
		if err != nil {
			return errors.New(err)
		}
		raws[k] = out
	}

	for k, raw := range raws {
		if _, err := m.Table.Insert(&rethinkSchema{
			KID: keys[k].KeyID,
			Set: set,
			Key: raw,
		}).RunWrite(m.Session); err != nil {
			return errors.New(err)
		}
	}

	return nil
}
func (m *RethinkManager) publishDeleteAll(set string) error {
	if err := m.Table.Filter(map[string]interface{}{
		"set": set,
	}).Delete().Exec(m.Session); err != nil {
		return errors.New(err)
	}
	return nil
}

func (m *RethinkManager) publishDelete(set string, keys []jose.JsonWebKey) error {
	for _, key := range keys {
		if _, err := m.Table.Filter(map[string]interface{}{
			"kid": key.KeyID,
			"set": set,
		}).Delete().RunWrite(m.Session); err != nil {
			return errors.New(err)
		}
	}
	return nil
}

func (m *RethinkManager) Watch(ctx context.Context) error {
	connections, err := m.Table.Changes().Run(m.Session)
	if err != nil {
		return errors.New(err)
	}

	go func() {
		for {
			var update map[string]*rethinkSchema
			for connections.Next(&update) {
				newVal := update["new_val"]
				oldVal := update["old_val"]
				m.Lock()
				if newVal == nil && oldVal != nil {
					m.watcherRemove(oldVal)

				} else if newVal != nil && oldVal != nil {
					m.watcherRemove(oldVal)
					m.watcherInsert(newVal)
				} else {
					m.watcherInsert(newVal)
				}
				m.Unlock()
			}

			connections.Close()
			if connections.Err() != nil {
				pkg.LogError(errors.New(connections.Err()))
			}

			connections, err = m.Table.Changes().Run(m.Session)
			if err != nil {
				pkg.LogError(errors.New(connections.Err()))
			}
		}
	}()

	return nil
}

func (m *RethinkManager) watcherInsert(val *rethinkSchema) {
	var c jose.JsonWebKey
	if err := json.Unmarshal(val.Key, &c); err != nil {
		panic(err)
	}

	keys := m.Keys[val.Set]
	keys.Keys = append(keys.Keys, c)
	m.Keys[val.Set] = keys
}

func (m *RethinkManager) watcherRemove(val *rethinkSchema) {
	keys, ok := m.Keys[val.Set]
	if !ok {
		return
	}

	keys.Keys = filter(keys.Keys, func(k jose.JsonWebKey) bool {
		return k.KeyID != val.KID
	})
	m.Keys[val.Set] = keys
}

func (m *RethinkManager) ColdStart() error {
	m.Keys = map[string]jose.JsonWebKeySet{}
	clients, err := m.Table.Run(m.Session)
	if err != nil {
		return errors.New(err)
	}

	var raw *rethinkSchema
	var key jose.JsonWebKey
	m.Lock()
	defer m.Unlock()
	for clients.Next(&raw) {
		if err := json.Unmarshal(raw.Key, &key); err != nil {
			return errors.New(err)
		}

		keys, ok := m.Keys[raw.Set]
		if !ok {
			keys = jose.JsonWebKeySet{}
		}
		keys.Keys = append(keys.Keys, key)
		m.Keys[raw.Set] = keys
	}

	return nil
}

func filter(vs []jose.JsonWebKey, f func(jose.JsonWebKey) bool) []jose.JsonWebKey {
	vsf := make([]jose.JsonWebKey, 0)
	for _, v := range vs {
		if f(v) {
			vsf = append(vsf, v)
		}
	}
	return vsf
}