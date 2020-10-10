module github.com/creachadair/ffuse

go 1.13

replace bazil.org/fuse => bazil.org/fuse v0.0.0-20200407214033-5883e5a4b512

require (
	bazil.org/fuse v0.0.0-20200524192727-fb710f7dfd05
	github.com/creachadair/badgerstore v0.0.7
	github.com/creachadair/boltstore v0.0.0-20201003170606-ae1eaff430c7
	github.com/creachadair/ffs v0.0.0-20201010032336-806306054c66
	github.com/creachadair/getpass v0.1.1
	github.com/creachadair/keyfile v0.5.3
	github.com/creachadair/sqlitestore v0.0.0-20201001181217-283d415e2ec5
	golang.org/x/crypto v0.0.0-20201002170205-7f63de1d35b0
	golang.org/x/net v0.0.0-20201002202402-0a1ea396d57c // indirect
	google.golang.org/protobuf v1.25.0
)
