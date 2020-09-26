module github.com/creachadair/ffuse

go 1.13

replace bazil.org/fuse => bazil.org/fuse v0.0.0-20200407214033-5883e5a4b512

require (
	bazil.org/fuse v0.0.0-20200524192727-fb710f7dfd05
	github.com/creachadair/badgerstore v0.0.6
	github.com/creachadair/boltstore v0.0.0-20200926215448-c8b0f8cd826a
	github.com/creachadair/ffs v0.0.0-20200926224219-48570a05dc23
	github.com/creachadair/getpass v0.1.0
	github.com/creachadair/keyfile v0.5.1
	google.golang.org/protobuf v1.25.0
)
