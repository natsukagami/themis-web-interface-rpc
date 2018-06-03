package main

import (
	"flag"
	"io/ioutil"
	"log"
	"strings"

	"github.com/natsukagami/themis-web-interface-rpc"

	// Use my fork because Windows screws up
	"github.com/natsukagami/go-input"
)

var (
	server = flag.String("server", "twi.nkagami.me:443", "The RPC server.")
)

func main() {
	flag.Parse()

	// Simple check whether we're in TWI's folder?
	if ver, err := ioutil.ReadFile("twi.version"); err != nil {
		log.Fatal("Hãy đảm bảo bạn đang chạy file này từ cùng thư mục của themis-web-interface!")
	} else {
		log.Println("Đã tìm thấy themis-web-interface " + string(ver) + ".")
	}

	ui := input.DefaultUI()
	key, err := ui.Ask("Hãy nhập key bạn đã nhận được khi thiết lập (để trống để tạo key mới)", &input.Options{
		Required: false,
	})
	if err != nil {
		panic(err)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		log.Println("Đang tạo cặp key mới cho Themis của bạn!")
		role, err := ui.Select("Bạn là một máy chấm (server) hay một máy chạy themis-web-interface (client) thôi?", []string{"server", "client"}, &input.Options{
			Required: true,
			Loop:     true,
		})
		if err != nil {
			panic(err)
		}
		client, key, err := themis.CreateKeys(*server, role)
		if err != nil {
			panic(err)
		}
		log.Println("Một cặp key mới được tạo thành công! Hãy lưu lại cặp key này để có thể sử dụng tiếp.")
		log.Println(" Máy chấm:", key.Server)
		log.Println(" Máy nộp bài:", key.Client)
		log.Println("Mã sẽ tự xóa sau 24 tiếng. Hãy chuyển mã cho người còn lại!")

		log.Println()
		log.Println("Chương trình đang chạy và sẽ cập nhật thư mục bài nộp và kết quả chấm với máy đối phương.")

		<-client.Done
	} else {
		client, err := themis.NewClient(*server, key)
		if err != nil {
			log.Fatalf("%+v", err)
		}

		log.Println()
		log.Println("Chương trình đang chạy và sẽ cập nhật thư mục bài nộp và kết quả chấm với máy đối phương.")

		<-client.Done
	}
}
