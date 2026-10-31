package main

import (
	"context"
	"log"

	pb "github.com/example/users/pb"
	"google.golang.org/grpc"
)

func main() {
	conn, _ := grpc.Dial("localhost:50051", grpc.WithInsecure())
	client := pb.NewUserServiceClient(conn)

	resp, err := client.GetUser(context.Background(), &pb.GetUserRequest{Id: "123"})
	if err != nil {
		log.Fatal(err)
	}
	log.Println(resp.Name)
}
