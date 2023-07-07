#
# This Dockerfile used to build a recent curl with HTTP/2 client support, using
# a recent nghttp2 build.
# Now it just wraps the latest curl build, which has all the requirements.
#
# See the Makefile for how to tag it. If Docker and that image is found, the
# Go tests use this curl binary for integration tests.
#

FROM curlimages/curl
