gobin = "/usr/local/go/bin/go"
mass-compress: server.go
	$(gobin) build

/usr/local/bin/mass-compress: mass-compress
	- service mass-compress stop
	cp ./mass-compress /usr/local/bin/
	service mass-compress start

/etc/init/mass-compress.conf: mass-compress.conf
	cp ./mass-compress.conf /etc/init/

install: /etc/init/mass-compress.conf /usr/local/bin/mass-compress

uninstall:
	rm /etc/init/mass-compress.conf /usr/local/bin/mass-compress

clean:
	rm mass-compress

.PHONY: clean install uninstall
