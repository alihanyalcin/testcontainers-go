# HTTP(S) Wait strategy

The HTTP wait strategy will check the result of an HTTP(S) request against the container and allows to set the following conditions:

- the port to be used.
- the path to be used.
- the HTTP method to be used.
- the HTTP request body to be sent.
- the HTTP status code matcher as a function.
- the HTTP response matcher as a function.
- the TLS config to be used for HTTPS.
- the startup timeout to be used in seconds, default is 60 seconds.
- the poll interval to be used in milliseconds, default is 100 milliseconds.
- the basic auth credentials to be used.

Variations on the HTTP wait strategy are supported, including:

## Match an HTTP method

<!--codeinclude-->
[Waiting for an HTTP endpoint](../../../wait/http_test.go) inside_block:waitForHTTP
<!--/codeinclude-->

## Match an HTTP method with Basic Auth

<!--codeinclude-->
[Waiting for an HTTP endpoint with Basic Auth](../../../wait/http_test.go) inside_block:waitForBasicAuth
<!--/codeinclude-->

## Match an HTTPS status code and a response matcher

<!--codeinclude-->
[Waiting for an HTTP endpoint matching an HTTP status code](../../../wait/http_test.go) inside_block:waitForHTTPStatusCode
<!--/codeinclude-->
