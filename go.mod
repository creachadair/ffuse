module github.com/creachadair/ffuse

go 1.13

replace bazil.org/fuse => bazil.org/fuse v0.0.0-20200407214033-5883e5a4b512

require (
	bazil.org/fuse v0.0.0-20200524192727-fb710f7dfd05
	github.com/DataDog/zstd v1.4.5 // indirect
	github.com/creachadair/badgerstore v0.0.6
	github.com/creachadair/boltstore v0.0.0-20200926215448-c8b0f8cd826a
	github.com/creachadair/ffs v0.0.0-20200928032611-ca65e981e6fa
	github.com/creachadair/getpass v0.1.1
	github.com/creachadair/keyfile v0.5.2
	github.com/dgraph-io/badger/v2 v2.2007.2 // indirect
	github.com/dgraph-io/ristretto v0.0.3 // indirect
	github.com/dgryski/go-farm v0.0.0-20200201041132-a6ae2369ad13 // indirect
	golang.org/x/net v0.0.0-20200927032502-5d4f70055728 // indirect
	google.golang.org/protobuf v1.25.0
)
