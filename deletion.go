package themis

import (
	"context"
	"time"

	"github.com/mongodb/mongo-go-driver/bson"
	"github.com/mongodb/mongo-go-driver/mongo"
	"github.com/pkg/errors"
)

func (s *Server) handleWatchKey(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var key Key
	err := s.KeySet.FindOne(ctx, bson.NewDocument(bson.EC.String("_id", id))).Decode(&key)
	if err == mongo.ErrNoDocuments {
		return nil // Somehow
	} else if err != nil {
		return errors.WithStack(err)
	}

	// Delete the key
	if _, err := s.KeySet.DeleteOne(ctx, bson.NewDocument(bson.EC.String("_id", id))); err != nil {
		return errors.WithStack(err)
	}

	// Delete its mapping
	s.mu.Lock()
	close(s.updates[key.ClientKey])
	delete(s.updates, key.ClientKey)
	close(s.updates[key.ServerKey])
	delete(s.updates, key.ServerKey)
	s.mu.Unlock()

	// Delete all the related files
	if _, err := s.FileSet.DeleteMany(ctx, map[string]interface{}{
		"$or": []interface{}{
			map[string]interface{}{"recvKey": key.ServerKey},
			map[string]interface{}{"recvKey": key.ClientKey},
		},
	}); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (s *Server) watchKeys() {
	ticker := time.NewTicker(ScanDuration)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		// Fetch all deletable keys
		cursor, err := s.KeySet.Find(ctx, map[string]interface{}{
			"$lte": map[string]interface{}{
				"lastUsed": time.Now().Add(-ScanDuration),
			},
		})
		if err != nil {
			s.reportInternal(err)
			cancel()
			continue
		}
		for cursor.Next(ctx) {
			var key Key
			if err := cursor.Decode(&key); err != nil {
				s.reportInternal(err)
				continue
			}
			if err := s.handleWatchKey(key.ServerKey); err != nil {
				s.reportInternal(err)
				continue
			}
		}
		cursor.Close(ctx)
		cancel()
	}
}
