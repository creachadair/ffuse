package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"

	"github.com/creachadair/binpack"
	"github.com/creachadair/ffs/file/wiretype"
)

func main() {
	data, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		log.Fatalf("Read: %v", err)
	}
	var pb wiretype.Node
	if err := binpack.Unmarshal(data, &pb); err != nil {
		log.Fatalf("Decode: %v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(&pb)
}
