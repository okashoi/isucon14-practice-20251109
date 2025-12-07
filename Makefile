APPNAME := isuride-go.service

.PHONY: *
gogo: stop-services build logs/clear start-services start-bench

stop-services:
	sudo systemctl stop nginx
	sudo systemctl stop $(APPNAME)
	sudo systemctl stop isuride-matcher.service 
	ssh isucon-s2 "sudo systemctl stop mysql"

build:
	cd go && go build -o isuride

logs: limit=10000
logs: opts=
logs:
	journalctl -ex --since "$(shell systemctl status isuride-go.service | grep "Active:" | awk '{print $$6, $$7}')" -n $(limit) -q $(opts)

logs/error:
	$(MAKE) logs opts='--grep "(error|panic|- 500)" --no-pager'

logs/clear:
	sudo journalctl --rotate && sudo journalctl --vacuum-size=1K
	sudo truncate --size 0 /var/log/nginx/access.log
	sudo truncate --size 0 /var/log/nginx/error.log
	ssh isucon-s2 "sudo truncate --size 0 /var/log/mysql/mysql-slow.log && sudo chmod 666 /var/log/mysql/mysql-slow.log"
	ssh isucon-s2 "sudo truncate --size 0 /var/log/mysql/error.log"

start-services:
	sudo systemctl daemon-reload
	ssh isucon-s2 "sudo systemctl start mysql"
	sudo systemctl start $(APPNAME)
	sudo systemctl start isuride-matcher.service 
	sudo systemctl start nginx

start-bench:
	ssh isucon-bench "./bench run . run --addr 172.31.6.255:443 --target https://isuride.xiv.isucon.net --payment-url http://172.31.2.189:12346 --payment-bind-port 12346 --skip-static-sanity-check"
