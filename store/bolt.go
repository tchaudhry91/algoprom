package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/boltdb/bolt"
	log "github.com/charmbracelet/log"
	"github.com/tchaudhry91/algoprom/actions"
	"github.com/tchaudhry91/algoprom/algochecks"
)

var ErrBucketEmpty = fmt.Errorf("Bucket Empty")
var ErrCheckNotFound = fmt.Errorf("Check not found")

type BoltStore struct {
	db     *bolt.DB
	logger *log.Logger
}

func NewBoltStore(f string, logger *log.Logger) (*BoltStore, error) {
	db, err := bolt.Open(f, 0600, nil)
	if err != nil {
		return nil, err
	}
	s := &BoltStore{
		db:     db,
		logger: logger,
	}
	return s, nil
}

func (s *BoltStore) PutCheck(ctx context.Context, check *algochecks.Check, output *algochecks.Output) (key string, err error) {
	key = ""
	name := check.Name
	err = s.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("checks"))
		if err != nil {
			return err
		}
		bucket, err = bucket.CreateBucketIfNotExists([]byte(name))
		if err != nil {
			return err
		}
		key = strconv.FormatInt(output.Timestamp.Unix(), 10)
		val, err := json.Marshal(output)
		if err != nil {
			return fmt.Errorf("Error Marshalling Output to JSON: %v", err)
		}
		bucket.Put([]byte(key), val)
		return nil
	})
	return key, err
}

func (s *BoltStore) GetCheck(ctx context.Context, name string, key string) (output *algochecks.Output, err error) {
	output = &algochecks.Output{}
	err = s.db.View(func(tx *bolt.Tx) error {
		checksBucket := tx.Bucket([]byte("checks"))
		if checksBucket == nil {
			return ErrBucketEmpty
		}
		bucket := checksBucket.Bucket([]byte(name))
		if bucket == nil {
			return ErrBucketEmpty
		}
		val := bucket.Get([]byte(key))
		if val == nil {
			return ErrCheckNotFound
		}
		err = json.Unmarshal(val, output)
		if err != nil {
			return fmt.Errorf("Could not marshal check JSON:%v", err)
		}
		return nil
	})
	return output, err
}

func (s *BoltStore) GetAllCheckNames(ctx context.Context) ([]string, error) {
	buckets := []string{}
	err := s.db.View(func(tx *bolt.Tx) error {
		checksBucket := tx.Bucket([]byte("checks"))
		if checksBucket == nil {
			return ErrBucketEmpty
		}
		checksBucket.ForEach(func(k []byte, v []byte) error {
			if v == nil {
				buckets = append(buckets, string(k))
			}
			return nil
		})
		return nil
	})
	return buckets, err
}

func (s *BoltStore) GetNamedCheck(ctx context.Context, name string, limit int) (allOutputs []algochecks.Output, err error) {
	allOutputs = []algochecks.Output{}
	err = s.db.View(func(tx *bolt.Tx) error {
		checksBucket := tx.Bucket([]byte("checks"))
		if checksBucket == nil {
			return ErrBucketEmpty
		}
		checkBucket := checksBucket.Bucket([]byte(name))
		if checkBucket == nil {
			return nil
		}
		cur := checkBucket.Cursor()
		count := 0
		k, v := cur.First()
		for count < limit {
			if k == nil {
				break
			}
			output := algochecks.Output{}
			err = json.Unmarshal(v, &output)
			if err != nil {
				return err
			}
			allOutputs = append(allOutputs, output)
			k, v = cur.Next()
			count += 1
		}
		return nil
	})
	return allOutputs, err
}

func (s *BoltStore) PutAction(ctx context.Context, checkName string, action *actions.ActionMeta, output *actions.Output) (key string, err error) {
	key = checkName + "_" + action.Name + "_"
	err = s.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("actions"))
		if err != nil {
			return err
		}
		key += strconv.FormatInt(output.Timestamp.Unix(), 10)
		val, err := json.Marshal(output)
		if err != nil {
			return fmt.Errorf("Error Marshalling Output to JSON: %v", err)
		}
		bucket.Put([]byte(key), val)
		return nil
	})
	return key, err
}
