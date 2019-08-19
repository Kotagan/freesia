package freesia

import (
	"context"
	"fmt"

	"github.com/xiaojiaoyu100/lizard/mass"

	"github.com/go-redis/redis"
	"github.com/pkg/errors"
	"github.com/vmihailenco/msgpack"
	"github.com/xiaojiaoyu100/curlew"
	"github.com/xiaojiaoyu100/freesia/entry"
	"github.com/xiaojiaoyu100/roc"
)

type Freesia struct {
	store      Store
	cache      *roc.Cache
	dispatcher *curlew.Dispatcher
}

func New(store Store, setters ...Setter) (*Freesia, error) {
	var err error

	f := new(Freesia)
	f.store = store
	for _, setter := range setters {
		err = setter(f)
		if err != nil {
			return nil, err
		}
	}

	cache, err := roc.New()
	if err != nil {
		return nil, err
	}
	f.cache = cache

	monitor := func(err error) {}
	f.dispatcher, err = curlew.New(curlew.WithMonitor(monitor))
	if err != nil {
		return nil, err
	}

	f.sub()

	return f, nil
}

func (f *Freesia) Set(e *entry.Entry) error {
	if err := e.Encode(); err != nil {
		return errors.Wrapf(err, "encode key = %s, value = %+v", e.Key, e.Value)
	}
	if err := f.store.Set(e.Key, e.Data(), e.Expiration).Err(); err != nil {
		return errors.Wrapf(err, "store set key = %s, value = %+v", e.Key, e.Value)
	}
	if e.EnableLocalCache() {
		if err := f.cache.Set(e.Key, e.Data(), e.Expiration/2); err != nil {
			return errors.Wrapf(err, "cache set key = %s, value = %+v", e.Key, e.Value)
		}
	}
	return nil
}

func (f *Freesia) MSet(es ...*entry.Entry) error {
	pipe := f.store.Pipeline()
	for _, e := range es {
		if err := e.Encode(); err != nil {
			return errors.Wrapf(err, "encode key = %s, value = %+v", e.Key, e.Value)
		}
		pipe.Set(e.Key, e.Data(), e.Expiration)
	}
	_, err := pipe.Exec()
	if err != nil {
		return errors.Wrapf(err, "pipeline exec")
	}
	for _, e := range es {
		if e.EnableLocalCache() {
			err := f.cache.Set(e.Key, e.Data(), e.Expiration)
			if err != nil {
				return errors.Wrapf(err, "cache set key = %s, value = %+v", e.Key, e.Value)
			}
		}
	}
	return nil
}

func (f *Freesia) Get(e *entry.Entry) error {
	if e.EnableLocalCache() {
		data, err := f.cache.Get(e.Key)
		if err == nil {
			b, ok := data.([]byte)
			if err := e.Decode(b); ok && err != nil {
				return errors.Wrapf(err, "decode key = %s, data = %s", e.Key, b)
			}
			return nil
		}
	}
	b, err := f.store.Get(e.Key).Bytes()
	switch err {
	case redis.Nil:
		j := curlew.NewJob()
		j.Arg = e
		j.Fn = func(ctx context.Context, arg interface{}) error {
			return f.Set(arg.(*entry.Entry))
		}
		f.dispatcher.SubmitAsync(j)
		return err
	case nil:
		err = e.Decode(b)
		if err != nil {
			return errors.Wrapf(err, "decode key = %s, data = %s", e.Key, b)
		}
	default:
		return errors.Wrapf(err, "store get key = %s", e.Key)
	}

	return nil
}

func (f *Freesia) batchGet(es ...*entry.Entry) ([]*entry.Entry, error) {
	pipe := f.store.Pipeline()
	found := make(map[*entry.Entry]struct{})
	ret := make(map[*redis.StringCmd]*entry.Entry)
	for _, e := range es {
		if e.EnableLocalCache() {
			b, err := f.cache.Get(e.Key)
			if data, ok := b.([]byte); ok && err == nil {
				err := e.Decode(data)
				return nil, err
			}
			found[e] = struct{}{}
		} else {
			cmd := pipe.Get(e.Key)
			ret[cmd] = e
		}
	}
	cmders, err := pipe.Exec()
	if err != nil {
		return nil, err
	}

	for _, cmder := range cmders {
		cmd, ok := cmder.(*redis.StringCmd)
		if !ok {
			continue
		}
		e, ok := ret[cmd]
		if !ok {
			continue
		}
		b, err := cmd.Bytes()
		switch err {
		case redis.Nil:
			j := curlew.NewJob()
			j.Arg = e
			j.Fn = func(ctx context.Context, arg interface{}) error {
				return f.Set(arg.(*entry.Entry))
			}
			f.dispatcher.SubmitAsync(j)
		case nil:
			err = e.Decode(b)
			if err != nil {
				return nil, err
			}
			found[e] = struct{}{}
		default:
			return nil, err
		}
	}
	missEntries := make([]*entry.Entry, 0, len(es))
	for _, e := range es {
		_, ok := found[e]
		if !ok {
			missEntries = append(missEntries, e)
		}
	}
	return missEntries, nil
}

func (f *Freesia) MGet(es ...*entry.Entry) ([]*entry.Entry, error) {
	batch := mass.New(len(es), 3000)
	missEntries := make([]*entry.Entry, 0, len(es))
	var start, length int
	for batch.Iter(&start, &length) {
		ee, err := f.batchGet(es[start : start+length]...)
		if err != nil {
			return nil, err
		}
		missEntries = append(missEntries, ee...)
	}
	return missEntries, nil
}

func (f *Freesia) Del(keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := f.store.Del(keys...).Result()
	if err != nil {
		return errors.Wrapf(err, "store del, keys = %+v", keys)
	}
	for _, key := range keys {
		if err := f.cache.Del(key); err != nil {
			return errors.Wrapf(err, "delete cache: key = %s", key)
		}
	}
	return nil
}

func (f *Freesia) sub() {
	go func() {
		pubSub := f.store.Subscribe(channel)
		defer func() {
			if err := pubSub.Close(); err != nil {
				fmt.Printf("pubsub err = %#v", err)
			}
		}()
		for message := range pubSub.Channel() {
			job := curlew.NewJob()
			job.Arg = message
			job.Fn = func(ctx context.Context, arg interface{}) error {
				message := arg.(*redis.Message)
				var keys []string
				if err := msgpack.Unmarshal([]byte(message.Payload), &keys); err != nil {
					return err
				}
				for _, key := range keys {
					if err := f.cache.Del(key); err != nil {
						fmt.Printf("CacheDeleteKey key = %s, err = %#v", key, err)
					}
				}
				return nil
			}
			f.dispatcher.SubmitAsync(job)
		}
	}()

}
