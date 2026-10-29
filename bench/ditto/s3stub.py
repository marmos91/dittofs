#!/usr/bin/env python3
"""s3stub.py PORT: an S3 endpoint on 127.0.0.1:PORT whose one bucket exists and is empty.

night.sh answers the block store's endpoint with it only while it sets DittoFS up, since creating
a share waits for the store's health probe. The measured server then starts with nothing on that
port, so its sync to the block store stays paused while the journal takes the writes.
"""
import http.server
import sys

EMPTY = b'<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>'
MISSING = b'<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code></Error>'


class Handler(http.server.BaseHTTPRequestHandler):
    def reply(self, code, body=b""):
        self.send_response(code)
        self.send_header("Content-Type", "application/xml")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def do_HEAD(self):
        self.reply(200)

    def do_GET(self):
        # A path of the bucket alone, with or without a query, is a listing.
        bucket_only = self.path.split("?")[0].strip("/").count("/") == 0
        self.reply(200, EMPTY) if bucket_only else self.reply(404, MISSING)

    def do_PUT(self):
        self.reply(200)

    do_POST = do_DELETE = do_PUT

    def log_message(self, fmt, *args):
        sys.stderr.write("s3stub: " + (fmt % args) + "\n")


http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
