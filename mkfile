all:V: test
	echo 'ollie-core: ok'

test:V:
	go test ./...
