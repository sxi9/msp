#!/bin/sh
# A harmless marker script used by the deploy run-path test: it prints a line
# the test asserts on and, given a path argument, creates that marker file so the
# test can prove the deployed file was actually executed. It touches nothing else
# and installs nothing.
echo "deploy-marker ran with arg=$1"
if [ -n "$1" ]; then
	: > "$1"
fi
