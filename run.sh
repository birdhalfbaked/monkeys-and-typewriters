#!/bin/sh
sysctl net.ipv4.ip_local_port_range="15000 61000"
sysctl net.ipv4.tcp_fin_timeout=30
go run cmd/stresser/main.go
