GOT_CONFIG=goterra-acct.yml go run goterra-acct.go



# influxdb

select sum(quantity) from "goterra.acct" group by "ns","resource","kind", time(30m);


docker run -p 8086:8086 -e INFLUXDB_DB=goterra -e INFLUXDB_USER=goterra -e INFLUXDB_USER_PASSWORD=goterra influxdb


docker run \
  -d \
  -p 3000:3000 \
  -e "GF_SECURITY_ADMIN_PASSWORD=secret" \
  grafana/grafana

