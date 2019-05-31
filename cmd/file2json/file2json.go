package main

import (
	"io"
	"io/ioutil"
	"log"
	"os"

	"bitbucket.org/creachadair/ffs/file/wirepb"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
)

func main() {
	data, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		log.Fatalf("Read: %v", err)
	}
	var pb wirepb.Node
	if err := proto.Unmarshal(data, &pb); err != nil {
		log.Fatalf("Decode: %v", err)
	}
	var m jsonpb.Marshaler
	if err := m.Marshal(os.Stdout, &pb); err != nil {
		log.Fatalf("Encode: %v", err)
	}
	io.WriteString(os.Stdout, "\n")
}
