package themis

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/fsnotify/fsnotify"
	proto "github.com/natsukagami/themis-web-interface-rpc/proto"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

// Directory paths for logs and submissions
var (
	submissionsPath = filepath.Join("data", "submit")
	logsPath        = filepath.Join(submissionsPath, "logs")
)

// Client is a wrapper over Proto's client.
type Client struct {
	proto.RPCClient
	Key  string
	Role string // `server` or `client`

	updates <-chan *Update
	Error   error

	Done <-chan int
}

func (c *Client) filterFilename(filename string) bool {
	path := filepath.Clean(filename)
	fmt.Println(path, filename)
	if filepath.Dir(path) != submissionsPath && filepath.Dir(path) != logsPath {
		return false // Could be an invalid file
	}
	return true
}

func (c *Client) handleUpdate(u *Update) error {
	log.Println("Update received:", u.Type, u.Message)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	switch u.Type {
	case UpdateGetFile:
		// You should fetch a file from the server
		file, err := c.ReceiveFile(ctx, &proto.Metadata{Key: c.Key, Checksum: u.Message})
		if err != nil {
			return errors.WithStack(err)
		}

		if !c.filterFilename(file.Filename) {
			return nil
		}

		if err := ioutil.WriteFile(file.Filename, file.Content, 0755); err != nil {
			return errors.WithStack(err)
		}
	case UpdateDelFile:
		if !c.filterFilename(u.Message) {
			return nil
		}

		if err := os.Remove(u.Message); err != nil {
			return errors.WithStack(err)
		}
	default:
		return errors.Errorf("Invalid type", u.Type)
	}

	return nil
}

// ReceiveUpdates receives all updates from the Server and act accordingly.
func (c *Client) ReceiveUpdates() {
	for update := range c.updates {
		if err := c.handleUpdate(update); err != nil {
			log.Println("Error on update", err)
		}
	}
}

func (c *Client) filterEvent(e fsnotify.Event) bool {
	// For clients and servers, we have different flavors for what to watch
	if c.Role == "server" {
		if strings.HasPrefix(e.Name, logsPath) {
			return e.Op == fsnotify.Create || e.Op == fsnotify.Write
		}
		return e.Op == fsnotify.Remove
	}
	if strings.HasPrefix(e.Name, logsPath) {
		return false
	}
	return e.Op == fsnotify.Create || e.Op == fsnotify.Write
}

func (c *Client) handleEvent(e fsnotify.Event) error {
	if !c.filterEvent(e) {
		return nil // We don't need to send this event
	}

	log.Println("Sending event", e.Op, "from file", e.Name)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Now send it
	if e.Op == fsnotify.Remove {
		if _, err := c.DeleteFile(ctx, &proto.File{
			Key:      c.Key,
			Filename: e.Name,
		}); err != nil {
			return errors.WithStack(err)
		}
	} else {
		if s, err := os.Stat(e.Name); err != nil {
			return errors.WithStack(err)
		} else if s.IsDir() {
			return nil // Don't handle directories
		} else if s.Size() > 10*1024 {
			// If the file's size is too large, keep it away
			log.Printf("File %s too large, ignoring\n", e.Name)
			return nil
		}

		content, err := ioutil.ReadFile(e.Name)
		if err != nil {
			return errors.WithStack(err)
		}
		if _, err := c.SendFile(ctx, &proto.File{
			Key:      c.Key,
			Filename: e.Name,
			Content:  content,
		}); err != nil {
			return errors.WithStack(err)
		}
	}

	log.Println("File sent")
	return nil
}

// PushUpdates watches the changes in the filesystem and report them!
func (c *Client) PushUpdates() {
	// First create the directories beforehand
	if err := os.MkdirAll(logsPath, 0755); err != nil {
		panic(err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err)
	}
	defer watcher.Close()
	watcher.Add(logsPath)
	watcher.Add(submissionsPath)

	for {
		select {
		case e := <-watcher.Events:
			if err := c.handleEvent(e); err != nil {
				log.Println("Error while handling event", err)
			}
		case e := <-watcher.Errors:
			log.Println("Error while watching files", e)
		}
	}
}

// NewClient creates a new Client and connects to the service.
func NewClient(url string, key string) (*Client, error) {
	conn, err := grpc.Dial(url, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	if err != nil {
		return nil, errors.WithStack(err)
	}
	c := &Client{
		RPCClient: proto.NewRPCClient(conn),
		Key:       key,
	}

	updateCh := make(chan *Update)
	c.updates = updateCh

	doneCh := make(chan int)
	c.Done = doneCh

	// Since we have the keys, just run fetchUpdates
	client, err := c.FetchUpdates(context.Background(), &proto.Empty{Key: key})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	go func() {
		defer func() { doneCh <- 1 }()
		// Post all updates to update!
		for {
			upd, err := client.Recv()
			if err != nil {
				close(updateCh)
				if err != io.EOF {
					c.Error = err
				}
				return
			}
			updateCh <- &Update{
				Type:    upd.Type,
				Message: upd.Message,
			}
		}
	}()

	// Receive the role of our client
	roleUpd, ok := <-c.updates
	if !ok {
		return nil, errors.WithStack(c.Error)
	}
	if roleUpd.Type != UpdateRole {
		return nil, errors.New("Cannot receive role type")
	}
	c.Role = roleUpd.Message

	go c.ReceiveUpdates()
	go c.PushUpdates()

	return c, nil
}

// CreateKeys create a client and create a key on the process.
func CreateKeys(url string, role string) (*Client, *proto.Key, error) {
	conn, err := grpc.Dial(url, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	client := proto.NewRPCClient(conn)

	key, err := client.CreateKey(context.Background(), &proto.Empty{})
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	var usedKey string
	if role == "server" {
		usedKey = key.Server
	} else {
		usedKey = key.Client
	}

	c, err := NewClient(url, usedKey)

	return c, key, err
}
