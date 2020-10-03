module github.com/creachadair/ffuse

go 1.13

replace bazil.org/fuse => bazil.org/fuse v0.0.0-20200407214033-5883e5a4b512

require (
	bazil.org/fuse v0.0.0-20200524192727-fb710f7dfd05
	github.com/DataDog/zstd v1.4.5 // indirect
	github.com/creachadair/badgerstore v0.0.6
	github.com/creachadair/boltstore v0.0.0-20201001170538-6a92c1d09a76
	github.com/creachadair/ffs v0.0.0-20201003163401-61da5edfd92c
	github.com/creachadair/getpass v0.1.1
	github.com/creachadair/keyfile v0.5.2
	github.com/creachadair/sqlitestore v0.0.0-20201001181217-283d415e2ec5
	github.com/dgraph-io/badger/v2 v2.2007.2 // indirect
	github.com/dgraph-io/ristretto v0.0.3 // indirect
	github.com/dgryski/go-farm v0.0.0-20200201041132-a6ae2369ad13 // indirect
	golang.org/x/crypto v0.0.0-20201002170205-7f63de1d35b0
	golang.org/x/net v0.0.0-20200930145003-4acb6c075d10 // indirect
	golang.org/x/sys v0.0.0-20200930185726-fdedc70b468f // indirect
	google.golang.org/protobuf v1.25.0
)
