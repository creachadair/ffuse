module github.com/creachadair/ffuse

go 1.13

replace bazil.org/fuse => bazil.org/fuse v0.0.0-20200407214033-5883e5a4b512

require (
	bazil.org/fuse v0.0.0-20200524192727-fb710f7dfd05
	github.com/creachadair/binpack v0.0.8
	github.com/creachadair/ffs v0.0.0-20210330135354-d2fe618a7bf6
	github.com/creachadair/jrpc2 v0.17.0
	github.com/creachadair/rpcstore v0.0.0-20210212170421-ab45512f6769
	golang.org/x/sys v0.0.0-20201107080550-4d91cf3a1aaf // indirect
)
