package pubsub

import (
	"context"

	"github.com/go-redis/redis/v8"

	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
)

type PubSub struct {
	rdb *redis.Client
}

func NewPubSub(redisClient *redis.Client) *PubSub {
	return &PubSub{
		rdb: redisClient,
	}
}

func (p *PubSub) Publish(ctx context.Context, channelName string, message string) error {
	return p.rdb.Publish(ctx, channelName, message).Err()
}

// To prevent resource leakage, you should close the subscriber when done.
// For example:
//  subscriber := ps.Subscribe(ctx, channelName)
//  defer subscriber.Close()
//  for m := range subscriber.Chan() {
//    // GOT CALLBACK!
//  }
func (p *PubSub) Subscribe(ctx context.Context, channelName string) interfaces.Subscriber {
	return &Subscriber{
		ps: p.rdb.Subscribe(ctx, channelName),
	}
}

type Subscriber struct {
	ps *redis.PubSub
}

func (s *Subscriber) Close() error {
	return s.ps.Close()
}

func (s *Subscriber) Chan() <-chan string {
	internalChannel := s.ps.Channel()
	externalChannel := make(chan string)
	go func() {
		for m := range internalChannel {
			externalChannel <- m.Payload
		}
	}()
	return externalChannel
}
