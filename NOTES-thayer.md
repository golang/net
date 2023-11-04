go test -v golang.org/x/net/http2 -run "TestTransport.*" >&1 | tee bench-trans.out


go test -v golang.org/x/net/http2 -run ".*Transport.*" -bench ".*Transport.*" >&1 | tee bench-trans.out


go test -v golang.org/x/net/http2 -run="^$" -bench=".*Transport.*" -count=10 >&1 | tee bench-trans2.out
