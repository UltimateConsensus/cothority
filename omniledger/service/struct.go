package service

import (
	"errors"
	"fmt"
	"sync"

	bolt "github.com/coreos/bbolt"
	"github.com/dedis/cothority/omniledger/collection"
	"github.com/dedis/cothority/omniledger/darc"
	"github.com/dedis/cothority/skipchain"
	"github.com/dedis/onet/network"
)

func init() {
	network.RegisterMessages(&darc.Signature{},
		DataHeader{}, DataBody{})
}

type collectionDB struct {
	db         *bolt.DB
	bucketName []byte
	coll       *collection.Collection
	scID       skipchain.SkipBlockID
}

// A CollectionView is an interface that defines the read-only operations
// on a collection.
type CollectionView interface {
	// Get returns the collection.Getter for the given key.
	// collection.Getter is valid even for a non-existing key.
	Get(key []byte) collection.Getter
	// GetValues returns the value and the contractID of the given key, or
	// an error if something went wrong. A non-existing key returns an
	// error.
	GetValues(key []byte) (value []byte, contractID string, darcID darc.ID, err error)
	// GetSkipchainID gets the skipchain ID that this collection view
	// represents.
	GetSkipchainID() skipchain.SkipBlockID
}

// roCollection is a wrapper for a collection that satisfies interface
// CollectionView and makes it impossible for callers who receive it to call
// the methods on the collection which can modify it. This is about type
// safety, not real security. If the holder of the CollectionView chooses to
// use package unsafe, then it's all over; they can get write access.
type roCollection struct {
	c    *collection.Collection
	scID skipchain.SkipBlockID
}

// Get returns the collection.Getter for the key.
func (r *roCollection) Get(key []byte) collection.Getter {
	return r.c.Get(key)
}

// GetValues returns the value of the key and the contractID. If the key
// does not exist, it returns an error.
func (r *roCollection) GetValues(key []byte) (value []byte, contractID string, darcID darc.ID, err error) {
	record, err := r.c.Get(key).Record()
	if err != nil {
		return
	}
	return getValuesFromRecord(record, key)
}

func (r *roCollection) GetSkipchainID() skipchain.SkipBlockID {
	if r.scID == nil {
		panic("if skipchain ID is nil, then this function should not be called")
	}
	return r.scID
}

// OmniLedgerContract is the type signature of the class functions
// which can be registered with the OmniLedger service.
// Since the outcome of the verification depends on the state of the collection
// which is to be modified, we pass it as a pointer here.
type OmniLedgerContract func(coll CollectionView, inst Instruction, inCoins []Coin) (sc []StateChange, outCoins []Coin, err error)

// newCollectionDB initialises a structure and reads all key/value pairs to store
// it in the collection.
func newCollectionDB(db *bolt.DB, name []byte) *collectionDB {
	c := &collectionDB{
		db:         db,
		bucketName: name,
		coll:       collection.New(collection.Data{}, collection.Data{}, collection.Data{}),
	}
	c.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket(name)
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}
		return nil
	})
	c.loadAll()
	// TODO: Check the merkle tree root.
	return c
}

// dup makes a copy of in. We use this with results from BoltDB
// because BoltDB's docs say, "The returned value is only valid for
// the life of the transaction."
func dup(in []byte) []byte {
	return append([]byte{}, in...)
}

func (c *collectionDB) loadAll() error {
	return c.db.View(func(tx *bolt.Tx) error {
		// Assume bucket exists and has keys
		b := tx.Bucket([]byte(c.bucketName))
		cur := b.Cursor()

		for k, v := cur.First(); k != nil; k, v = cur.Next() {
			// Only look at value keys
			if len(k) > 0 && k[0] != 0 {
				continue
			}

			k2 := dup(k)
			k2[0] = 1
			cv := b.Get(k2)
			if cv == nil {
				return fmt.Errorf("contract type missing for object ID %x", k[1:])
			}

			k2[0] = 2
			dv := b.Get(k2)
			if dv == nil {
				return fmt.Errorf("darcID missing for object ID %x", k[1:])
			}

			err := c.coll.Add(dup(k[1:]), dup(v), dup(cv), dup(dv))
			if err != nil {
				return err
			}
		}

		return nil
	})
}

func storeInColl(coll *collection.Collection, t *StateChange) error {
	switch t.StateAction {
	case Create:
		return coll.Add(t.InstanceID, t.Value, t.ContractID, []byte(t.DarcID))
	case Update:
		return coll.Set(t.InstanceID, t.Value, t.ContractID, []byte(t.DarcID))
	case Remove:
		return coll.Remove(t.InstanceID)
	default:
		return errors.New("invalid state action")
	}
}

func (c *collectionDB) Get(key []byte) collection.Getter {
	return c.coll.Get(key)
}

func (c *collectionDB) GetValues(key []byte) (value []byte, contractID string, darcID darc.ID, err error) {
	record, err := c.coll.Get(key).Record()
	if err != nil {
		return
	}
	return getValuesFromRecord(record, key)
}

func (c *collectionDB) Store(sc *StateChange) error {
	if err := storeInColl(c.coll, sc); err != nil {
		return err
	}
	err := c.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(c.bucketName))

		// The 3 pieces of data (value, contract, darcid) are stored under three keys
		// starting with 0, 1, 2.
		key := make([]byte, 1+len(sc.InstanceID))
		copy(key[1:], sc.InstanceID)

		switch sc.StateAction {
		case Create, Update:
			key[0] = 0
			if err := bucket.Put(key, sc.Value); err != nil {
				return err
			}
			key[0] = 1
			if err := bucket.Put(key, sc.ContractID); err != nil {
				return err
			}
			key[0] = 2
			if err := bucket.Put(key, []byte(sc.DarcID)); err != nil {
				return err
			}
			return nil
		case Remove:
			key[0] = 0
			if err := bucket.Delete(key); err != nil {
				return err
			}
			key[0] = 1
			if err := bucket.Delete(key); err != nil {
				return err
			}
			key[0] = 2
			if err := bucket.Delete(key); err != nil {
				return err
			}
			return nil
		default:
			return errors.New("invalid state action")
		}
	})
	return err
}

func (c *collectionDB) GetValueContract(key []byte) ([]byte, []byte, error) {
	// getValueContract does not use skipchain ID, so we just set it to nil
	return getValueContract(&roCollection{c.coll, nil}, key)
}

// TODO this function can be merged with getValuesFromRecord
func getValueContract(coll CollectionView, key []byte) (value, contract []byte, err error) {
	proof, err := coll.Get(key).Record()
	if err != nil {
		return
	}
	hashes, err := proof.Values()
	if err != nil {
		return
	}
	if len(hashes) == 0 {
		err = errors.New("nothing stored under that key")
		return
	}
	value, ok := hashes[0].([]byte)
	if !ok {
		err = errors.New("the value is not of type []byte")
		return
	}
	contract, ok = hashes[1].([]byte)
	if !ok {
		err = errors.New("the contract is not of type []byte")
		return
	}
	return
}

// RootHash returns the hash of the root node in the merkle tree.
func (c *collectionDB) RootHash() []byte {
	return c.coll.GetRoot()
}

func getValuesFromRecord(record collection.Record, key []byte) (value []byte, contractID string, darcID darc.ID, err error) {
	values, err := record.Values()
	if err != nil {
		return
	}
	if len(values) != 3 {
		err = errors.New("values does not have 3 elements")
		return
	}

	var ok bool
	value, ok = values[0].([]byte)
	if !ok {
		err = errors.New("values[0] is not a slice of bytes")
		return
	}

	c, ok := values[1].([]byte)
	if !ok {
		err = errors.New("values[1] is not a slice of bytes")
		return
	}
	contractID = string(c)

	did, ok := values[2].([]byte)
	if !ok {
		err = errors.New("values[2] is not a slice of bytes")
		return
	}
	darcID = darc.ID(did)

	return
}

// tryHash returns the merkle root of the collection as if the key value pairs
// in the transactions had been added, without actually adding it.
func (c *collectionDB) tryHash(ts []StateChange) (mr []byte, rerr error) {
	for _, sc := range ts {
		err := c.coll.Add(sc.InstanceID, sc.Value, sc.ContractID, []byte(sc.DarcID))
		if err != nil {
			rerr = err
			return
		}
		// remove the pair after we got the merkle root.
		defer func(k []byte) {
			err = c.coll.Remove(k)
			if err != nil {
				rerr = err
				mr = nil
			}
		}(sc.InstanceID)
	}
	mr = c.coll.GetRoot()
	return
}

// RegisterContract stores the contract in a map and will
// call it whenever a contract needs to be done.
// GetService makes it possible to give either an `onet.Context` or
// `onet.Server` to `RegisterContract`.
func RegisterContract(s skipchain.GetService, kind string, f OmniLedgerContract) error {
	scs := s.Service(ServiceName)
	if scs == nil {
		return errors.New("Didn't find our service: " + ServiceName)
	}
	return scs.(*Service).registerContract(kind, f)
}

type olState struct {
	sync.Mutex
	// lastBlock is the last integrated block into the collection
	lastBlock map[string]skipchain.SkipBlockID
	// waitChannels will be informed by Service.updateCollection that a
	// given ClientTransaction has been included. updateCollection will
	// send true for a valid ClientTransaction and false for an invalid
	// ClientTransaction.
	waitChannels map[string]chan bool
}

func (ol *olState) setLast(sb *skipchain.SkipBlock) {
	ol.Lock()
	defer ol.Unlock()
	ol.lastBlock[string(sb.SkipChainID())] = sb.Hash
}

func (ol *olState) getLast(id skipchain.SkipBlockID) skipchain.SkipBlockID {
	ol.Lock()
	defer ol.Unlock()
	return ol.lastBlock[string(id)]
}

func (ol *olState) createWaitChannel(ctxHash []byte) chan bool {
	ol.Lock()
	defer ol.Unlock()
	ch := make(chan bool, 1)
	ol.waitChannels[string(ctxHash)] = ch
	return ch
}

func (ol *olState) informWaitChannel(ctxHash []byte, valid bool) {
	ol.Lock()
	defer ol.Unlock()
	ch := ol.waitChannels[string(ctxHash)]
	if ch != nil {
		ch <- valid
	}
}

func (ol *olState) deleteWaitChannel(ctxHash []byte) {
	ol.Lock()
	defer ol.Unlock()
	delete(ol.waitChannels, string(ctxHash))
}
