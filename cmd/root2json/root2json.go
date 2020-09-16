package main

import (
	"io"
	"io/ioutil"
	"log"
	"os"

	"github.com/creachadair/ffs/file/wirepb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func main() {
	data, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		log.Fatalf("Read: %v", err)
	}
	var pb wirepb.Root
	if err := proto.Unmarshal(data, &pb); err != nil {
		log.Fatalf("Decode: %v", err)
	}
	io.WriteString(os.Stdout, protojson.Format(&pb))
}
