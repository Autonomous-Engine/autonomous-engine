package main

import "fmt"

func main() {
	kb.Subscribe(TopicUserCreated, func(msg []byte) {
		fmt.Println("got user created event")
	})

	kb.Publish(ctx, TopicOrderPlaced, payload)
}
