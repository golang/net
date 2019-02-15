module golang.org/x/net/http2/h2demo

// TODO: Require a future pseudo-version of golang.org/x/net module
//       where h2demo has been carved out.
//       Can't do that now because that pseudo-version doesn't exist yet;
//       it's being created in this very change.

require (
	cloud.google.com/go v0.36.0
	go4.org v0.0.0-20190218023631-ce4c26f7be8e
	golang.org/x/build v0.0.0-20190311051652-b8db43dd7225
	golang.org/x/crypto v0.0.0-20190308221718-c2843e01d9a2
)

replace golang.org/x/net => ../..
