package ddtxn

import (
	"ddtxn/dlog"
	"errors"
	"flag"
	"hash"
	"hash/crc32"
	"log"
	"runtime/debug"
	"sync"

	"github.com/zond/gotomic"
)

type TID uint64
type Key [16]byte
type Value interface{}

var hasher hash.Hash32

func (k Key) Equals(t gotomic.Thing) bool {
	return t.(Key) == k
}

func (k Key) HashCode() uint32 {
	hasher.Reset()
	hasher.Write(k[:])
	return hasher.Sum32()
}

type Chunk struct {
	padding1 [128]byte
	sync.RWMutex
	rows     map[Key]*BRecord
	padding2 [128]byte
}

var UseRLocks = flag.Bool("rlock", true, "Use Rlocks\n")
var GStore = flag.Bool("gstore", false, "Use Gotomic Hash Map instead of Go maps\n")

var (
	ENOKEY   = errors.New("doppel: no key")
	EABORT   = errors.New("doppel: abort")
	ESTASH   = errors.New("doppel: stash")
	ENORETRY = errors.New("app error: no retry")
	EEXISTS  = errors.New("doppel: trying to create key which already exists")
)

const (
	CHUNKS = 256
)

// Global data
type Store struct {
	padding1        [128]byte
	store           []*Chunk
	gstore          *gotomic.Hash
	NChunksAccessed []int64
	dd              map[Key]bool
	any_dd          bool
	cand            *Candidates
	padding2        [128]byte
}

func (s *Store) DD() map[Key]bool {
	return s.dd
}

func NewStore() *Store {
	hasher = crc32.NewIEEE()
	x := make([]*OneStat, 0)
	sh := StatsHeap(x)
	s := &Store{
		store:           make([]*Chunk, CHUNKS),
		gstore:          gotomic.NewHash(),
		NChunksAccessed: make([]int64, CHUNKS),
		dd:              make(map[Key]bool),
		cand:            &Candidates{make(map[Key]*OneStat), &sh},
	}
	var bb byte

	for i := 0; i < CHUNKS; i++ {
		chunk := &Chunk{
			rows: make(map[Key]*BRecord),
		}
		bb = byte(i)
		s.store[bb] = chunk
	}
	return s
}

func (s *Store) getOrCreateTypedKey(k Key, v Value, kt KeyType) *BRecord {
	br, err := s.getKey(k)
	if err == ENOKEY {
		if *GStore {
			br, ok := s.gstore.Get(k)
			if !ok {
				br = MakeBR(k, v, kt)
				did := s.gstore.PutIfMissing(k, br)
				if !did {
					br, ok = s.gstore.Get(k)
					if !ok {
						log.Fatalf("Cannot put new key, but Get() says it isn't there %v\n", k)
					}
				}
			}
		} else {
			if !*UseRLocks {
				log.Fatalf("Should have preallocated keys if not locking chunks\n")
			}
			// Create key
			chunk := s.store[k[0]]
			var ok bool
			chunk.Lock()
			br, ok = chunk.rows[k]
			if !ok {
				br = MakeBR(k, v, kt)
				chunk.rows[k] = br
			}
			chunk.Unlock()
		}
	}
	return br
}

func (s *Store) CreateKey(k Key, v Value, kt KeyType) *BRecord {
	br := MakeBR(k, v, kt)
	if *GStore {
		s.gstore.Put(k, br)
	} else {
		chunk := s.store[k[0]]
		chunk.Lock()
		chunk.rows[k] = br
		chunk.Unlock()
	}
	return br
}

// The next two functions exist because we have to make sure the
// record is locked and inserted while holding the lock on the chunk.

func (s *Store) CreateLockedKey(k Key, kt KeyType) (*BRecord, error) {
	br := MakeBR(k, nil, kt)
	br.Lock()
	if *GStore {
		ok := s.gstore.PutIfMissing(k, br)
		if !ok {
			dlog.Printf("Key already exists %v\n", k)
			return nil, EEXISTS
		}
	} else {
		chunk := s.store[k[0]]
		chunk.Lock()
		_, ok := chunk.rows[k]
		if ok {
			chunk.Unlock()
			dlog.Printf("Key already exists %v\n", k)
			return nil, EEXISTS
		}
		chunk.rows[k] = br
		chunk.Unlock()
	}
	return br, nil
}

func (s *Store) CreateMuLockedKey(k Key, kt KeyType) (*BRecord, error) {
	br := MakeBR(k, nil, kt)
	br.SLock()
	if *GStore {
		ok := s.gstore.PutIfMissing(k, br)
		if !ok {
			dlog.Printf("Key already exists %v\n", k)
			return nil, EEXISTS
		}
	} else {
		chunk := s.store[k[0]]
		chunk.Lock()
		_, ok := chunk.rows[k]
		if ok {
			chunk.Unlock()
			dlog.Printf("Key already exists %v\n", k)
			return nil, EEXISTS
		}
		chunk.rows[k] = br
		chunk.Unlock()
	}
	return br, nil
}

func (s *Store) CreateMuRLockedKey(k Key, kt KeyType) (*BRecord, error) {
	br := MakeBR(k, nil, kt)
	br.SRLock()
	if *GStore {
		ok := s.gstore.PutIfMissing(k, br)
		if !ok {
			dlog.Printf("Key already exists %v\n", k)
			return nil, EEXISTS
		}
	} else {
		chunk := s.store[k[0]]
		chunk.Lock()
		_, ok := chunk.rows[k]
		if ok {
			chunk.Unlock()
			dlog.Printf("Key already exists %v\n", k)
			return nil, EEXISTS
		}
		chunk.rows[k] = br
		chunk.Unlock()
	}
	return br, nil
}

func (s *Store) SetInt32(br *BRecord, v int32, op KeyType) {
	switch op {
	case SUM:
		br.int_value += v
	case MAX:
		if v > br.int_value {
			br.int_value = v
		}
	}
}

func (s *Store) SetList(br *BRecord, ve Entry, op KeyType) {
	br.AddOneToRecord(ve)
}

func (s *Store) SetOO(br *BRecord, a int32, v Value, op KeyType) {
	if v != nil {
		if a > br.int_value || br.value == nil {
			br.int_value = a
			br.value = v
		}
	}
}

func (s *Store) Set(br *BRecord, v Value, op KeyType) {
	switch op {
	case SUM:
		br.int_value += v.(int32)
	case MAX:
		x := v.(int32)
		if x > br.int_value {
			br.int_value = v.(int32)
		}
	case WRITE:
		br.value = v
	case LIST:
		if v != nil {
			br.AddOneToRecord(v.(Entry))
		}
	case OOWRITE:
		if v != nil {
			x := v.(Overwrite)
			s.SetOO(br, x.i, x.v, OOWRITE)
		}
	}
}

func (s *Store) Get(k Key) (*BRecord, error) {
	return s.getKey(k)
}

func (s *Store) getKey(k Key) (*BRecord, error) {
	if len(k) == 0 {
		debug.PrintStack()
		log.Fatalf("[store] getKey(): Empty key\n")
	}
	//s.NChunksAccessed[k[0]]++
	if *GStore {
		x, err := s.gstore.Get(k)
		if !err {
			return nil, ENOKEY
		} else {
			return x.(*BRecord), nil
		}
	}
	if !*UseRLocks {
		x, err := s.getKeyStatic(k)
		return x, err
	}
	chunk := s.store[k[0]]
	if chunk == nil {
		log.Fatalf("[store] Didn't initialize chunk for key %v byte %v\n", k, k[0])
	}
	chunk.RLock()
	vr, ok := chunk.rows[k]
	if !ok || vr == nil {
		chunk.RUnlock()
		return vr, ENOKEY
	}
	chunk.RUnlock()
	return vr, nil
}

func (s *Store) getKeyStatic(k Key) (*BRecord, error) {
	if len(k) == 0 {
		log.Fatalf("[store] getKey(): Empty key\n")
	}
	chunk := s.store[k[0]]
	if chunk == nil {
		log.Fatalf("[store] Didn't initialize chunk for key %v byte %v\n", k, k[0])
	}
	vr, ok := chunk.rows[k]
	if !ok || vr == nil {
		return vr, ENOKEY
	}
	return vr, nil
}

func (s *Store) IsDD(k Key) bool {
	if !s.any_dd {
		return false
	}
	x, ok := s.dd[k]
	return x && ok
}
