package themis

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mongodb/mongo-go-driver/bson"
	"github.com/mongodb/mongo-go-driver/mongo"
	proto "github.com/natsukagami/themis-web-interface-rpc/proto"
	"github.com/natsukagami/themis-web-interface-rpc/randomstring"
	"github.com/pkg/errors"
)

// Some defined constants
const (
	KeyTTL       = 24 * time.Hour
	ScanDuration = time.Hour
	SizeLimit    = 20480

	KeyLen = 16
)

var _ = proto.RPCServer((*Server)(nil))

// Server is a struct implementing the RPC server interface.
type Server struct {
	KeySet  *mongo.Collection
	FileSet *mongo.Collection

	mu      sync.Mutex
	updates map[string]chan<- *Update

	keyExpire chan string // Watches keys for deletion
}

func (s *Server) reportInternal(err error) error {
	if err == nil {
		return nil
	}
	log.Printf("%+v\n", errors.WithStack(err))
	if status.Code(err) == codes.Unknown {
		return status.Error(codes.Internal, err.Error())
	}
	return err
}

// NewServer creates a new Server.
func NewServer(db *mongo.Database) (*Server, error) {
	s := &Server{
		KeySet:    db.Collection("keys"),
		FileSet:   db.Collection("files"),
		keyExpire: make(chan string),
		updates:   make(map[string]chan<- *Update),
	}

	// CreateIndexes
	if _, err := s.KeySet.Indexes().CreateOne(context.Background(), mongo.IndexModel{
		Keys: bson.NewDocument(bson.EC.Int32("clientKey", 1)),
	}); err != nil {
		return nil, errors.WithStack(err)
	}
	if _, err := s.KeySet.Indexes().CreateOne(context.Background(), mongo.IndexModel{
		Keys: bson.NewDocument(bson.EC.Int32("lastUsed", 1)),
	}); err != nil {
		return nil, errors.WithStack(err)
	}
	if _, err := s.FileSet.Indexes().CreateOne(context.Background(), mongo.IndexModel{
		Keys: bson.NewDocument(bson.EC.Int32("recvKey", 1)),
	}); err != nil {
		return nil, errors.WithStack(err)
	}

	go s.watchKeys()

	return s, nil
}

// Returns the filter that receives the correct Key.
func filterKey(key string) map[string]interface{} {
	return map[string]interface{}{"$or": []interface{}{
		map[string]interface{}{"_id": key},
		map[string]interface{}{"clientKey": key},
	}}
}

// VerifyKey verifies the sent key is valid.
// If it is, returns the recipient key / is the reqKey a server key?.
func (s *Server) VerifyKey(ctx context.Context, reqKey string) (string, bool, error) {
	var key Key
	if err := s.KeySet.FindOne(ctx, filterKey(reqKey)).Decode(&key); err != nil {
		if err == mongo.ErrNoDocuments {
			return "", false, status.Error(codes.NotFound, "Key not found")
		}
		// TODO: Report to Raven
		return "", false, s.reportInternal(err)
	}

	// If the key has expired, clean it from the server
	if key.LastUsed.Add(KeyTTL).Before(time.Now()) {
		s.handleWatchKey(key.ServerKey)
		return "", false, status.Error(codes.ResourceExhausted, "Key reached time to live limit")
	}
	return key.Recipient(reqKey), reqKey == key.ServerKey, nil
}

// FetchUpdates connects to a Client, streaming any updates onto it.
func (s *Server) FetchUpdates(in *proto.Empty, stream proto.RPC_FetchUpdatesServer) error {
	_, isServer, err := s.VerifyKey(stream.Context(), in.Key)
	if err != nil {
		return err
	}

	log.Printf("%#v\n", stream)

	// Create a new Update stream
	updateStream := make(chan *Update)
	// Saves the stream
	s.mu.Lock()
	if oldStream, ok := s.updates[in.Key]; ok {
		close(oldStream)
	}
	s.updates[in.Key] = updateStream
	s.mu.Unlock()

	// Send the role marker
	go func() {
		if isServer {
			updateStream <- &Update{Type: UpdateRole, Message: "server"}
		} else {
			updateStream <- &Update{Type: UpdateRole, Message: "client"}
		}
	}()

	// Defer a channel close and cleanup

	// ? Should we fetch all updates from the start? No, just drop the thing.

	// Now just read from it
	for update := range updateStream {
		if err := stream.Send(&proto.Update{
			Type:    update.Type,
			Message: update.Message,
		}); err != nil {
			return s.reportInternal(err)
		}
	}
	// Exit gracefully (or not??)
	return nil
}

// ReceiveFile allows one to receive a file from the server.
// After a succesful receival, the file will be deleted.
func (s *Server) ReceiveFile(ctx context.Context, in *proto.Metadata) (*proto.File, error) {
	if _, _, err := s.VerifyKey(ctx, in.Key); err != nil {
		return nil, err
	}

	var f File
	if err := s.FileSet.FindOne(ctx, map[string]interface{}{
		"_id":     in.Checksum,
		"recvKey": in.Key,
	}).Decode(&f); err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, status.Error(codes.NotFound, "File not found")
		}
		// TODO: Report to Raven
		return nil, s.reportInternal(err)
	}

	return &proto.File{
		Key:      f.RecvKey,
		Checksum: f.Checksum,
		Filename: f.Filename,
		Content:  f.Content,
	}, nil
}

func getChecksum(content []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(content))
}

// SendFile sends a file to the server.
// If there's a channel ready to receive, the transfer proceeds.
// If not, the transfer is cancelled, but the file will still be uploaded.
// An error will be returned if the transfer did not succeed.
func (s *Server) SendFile(ctx context.Context, in *proto.File) (*proto.Metadata, error) {

	recv, _, err := s.VerifyKey(ctx, in.Key)
	if err != nil {
		return nil, err
	}

	if len(in.Content) > 10*1024 {
		// Don't take files larger than 10KBs
	}

	var file = &File{
		Checksum: getChecksum(append(in.Content, []byte(recv+in.Filename)...)),
		Content:  in.Content,
		RecvKey:  recv,
		Filename: in.Filename,
	}

	if _, err := s.FileSet.UpdateOne(ctx, map[string]interface{}{
		"_id": file.Checksum,
	}, map[string]interface{}{"$set": file}, mongo.Opt.Upsert(true)); err != nil {
		// TODO: Raven!!!
		return nil, s.reportInternal(err)
	}

	// Send the update thru the channel
	go func() {
		s.mu.Lock()
		upd, ok := s.updates[recv]
		if ok {
			upd <- &Update{Type: UpdateGetFile, Message: file.Checksum}
		}
		s.mu.Unlock()
	}()

	return &proto.Metadata{
		Key:      recv,
		Checksum: file.Checksum,
	}, nil
}

// DeleteFile just sends a file deletion event.
func (s *Server) DeleteFile(ctx context.Context, in *proto.File) (*proto.Empty, error) {
	recv, _, err := s.VerifyKey(ctx, in.Key)
	if err != nil {
		return nil, err
	}
	// Send the update thru the channel
	go func() {
		s.mu.Lock()
		upd, ok := s.updates[recv]
		if ok {
			upd <- &Update{Type: UpdateDelFile, Message: in.Filename}
		}
		s.mu.Unlock()
	}()

	return &proto.Empty{}, nil
}

func (s *Server) generateKey(ctx context.Context, existingKeys ...string) (string, error) {
	for {
		key := randomstring.Get(KeyLen)
		ok := true
		for _, str := range existingKeys {
			if key == str {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		count, err := s.KeySet.Count(ctx, filterKey(key))
		if err != nil {
			return "", s.reportInternal(err)
		}
		if count == 0 {
			return key, nil
		}
	}
}

// CreateKey returns a new pair of keys.
func (s *Server) CreateKey(ctx context.Context, _ *proto.Empty) (*proto.Key, error) {
	clientKey, err := s.generateKey(ctx)
	if err != nil {
		return nil, err
	}
	serverKey, err := s.generateKey(ctx, clientKey)
	if err != nil {
		return nil, err
	}

	if _, err := s.KeySet.InsertOne(ctx, Key{
		ServerKey: serverKey,
		ClientKey: clientKey,
		LastUsed:  time.Now(),
	}); err != nil {
		return nil, s.reportInternal(err)
	}

	return &proto.Key{
		Server: serverKey,
		Client: clientKey,
	}, nil
}
