package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"

	"github.com/mongodb/mongo-go-driver/mongo"

	themis "github.com/natsukagami/themis-web-interface-rpc"
	proto "github.com/natsukagami/themis-web-interface-rpc/proto"
	"google.golang.org/grpc"
)

var (
	port     = flag.Int("port", 8084, "The port for the gRPC server to listen on")
	mongoURL = flag.String("mongodb", "", "The MongoDB Database URL")
)

func main() {
	flag.Parse()

	if *mongoURL == "" {
		panic("MongoDB URL is not set")
	}

	mongodb, err := mongo.NewClient(*mongoURL)
	if err != nil {
		panic(err)
	}
	if err := mongodb.Connect(context.Background()); err != nil {
		panic(err)
	}
	db := mongodb.Database("twi-rpc-server")

	server, err := themis.NewServer(db)
	if err != nil {
		panic(err)
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		panic(err)
	}

	log.Println("Listening on port", *port)

	// creds := credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})
	grpcServer := grpc.NewServer()
	proto.RegisterRPCServer(grpcServer, server)

	grpcServer.Serve(lis)
}
