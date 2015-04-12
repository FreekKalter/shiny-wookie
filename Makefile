gobin = "/home/fkalter/go/bin/go"
mass-compress: server.go
	$(gobin) build

/usr/local/bin/mass-compress: mass-compress
	cp ./mass-compress /usr/local/bin/

/etc/init/mass-compress.conf: mass-compress.conf
	cp ./mass-compress.conf /etc/init/

install: /etc/init/mass-compress.conf /usr/local/bin/mass-compress

clean:
	rm mass-compress

.PHONY: clean install
