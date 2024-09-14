#!/bin/sh
for i in seq 0 10; do
docker run --name "stress_$i" --privileged -d stress;
sleep 1;
done
