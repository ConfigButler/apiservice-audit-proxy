# Changelog

## [0.4.0](https://github.com/ConfigButler/apiservice-audit-proxy/compare/apiservice-audit-proxy-v0.3.0...apiservice-audit-proxy-v0.4.0) (2026-05-20)


### Features

* add logging to debug installations (indicating insecure situations, common working and faulty situations). ([48f5130](https://github.com/ConfigButler/apiservice-audit-proxy/commit/48f51303384d9da39c4b78dd2d50d48270e71443))
* breaking reshuffle of helm chart. Let's make it easier to setup/understand ([d392d37](https://github.com/ConfigButler/apiservice-audit-proxy/commit/d392d37b67759ffe789ea2354b0145837641ee36))
* impersonation pass through mode ([2cbbc01](https://github.com/ConfigButler/apiservice-audit-proxy/commit/2cbbc0185a362962013f4889cb0355ee1ee9898d))
* read configuration for APIService endpoint from cluster's `extension-apiserver-authentication` ConfigMap. This drops all configuration of that for security reasons (there is no reason for now to even have all these knobs). ([a73cfc7](https://github.com/ConfigButler/apiservice-audit-proxy/commit/a73cfc78d694dd225fa813c53bcb138ae4691432))


### Documentation

* first version of authz background ([a47cd8c](https://github.com/ConfigButler/apiservice-audit-proxy/commit/a47cd8cee826e4abc57de1610f271f5f13602ebd))
* getting things inline/updates ([1fce9e9](https://github.com/ConfigButler/apiservice-audit-proxy/commit/1fce9e9fca05f623e65e64061e8272fd2347379f))
* improved version with references ([301eb1d](https://github.com/ConfigButler/apiservice-audit-proxy/commit/301eb1d4b599c742b3c42485b9cc60c758abcd07))
* pruning docs and aligning ([489aa50](https://github.com/ConfigButler/apiservice-audit-proxy/commit/489aa50dff7b82e3e8300b99c8515da3d7b954d7))
* the latest improvements, and more elaborate on how it all should work together ([0266eb9](https://github.com/ConfigButler/apiservice-audit-proxy/commit/0266eb979e1e3d661f848f529b684322ca2e05fe))
* updated docs to new situation ([ef26bd3](https://github.com/ConfigButler/apiservice-audit-proxy/commit/ef26bd340b8405a976924487b49423aea6041a9b))

## [0.3.0](https://github.com/ConfigButler/apiservice-audit-proxy/compare/apiservice-audit-proxy-v0.2.1...apiservice-audit-proxy-v0.3.0) (2026-04-24)


### Features

* add e2e test to show the projects why ([18462dc](https://github.com/ConfigButler/apiservice-audit-proxy/commit/18462dca83a9a63e0cfe1e95282397210553c8cb))
* add example files and match exact fields ([764cc15](https://github.com/ConfigButler/apiservice-audit-proxy/commit/764cc15ef1d4b6fefc99343a2073cb31583ced47))
* as much as possible through helm ([4666616](https://github.com/ConfigButler/apiservice-audit-proxy/commit/4666616db62814710dc395f36070ebcbe3a8e20b))
* faster port-forwarding ([8755086](https://github.com/ConfigButler/apiservice-audit-proxy/commit/87550860820947613f5e79574986a841f808b263))
* replaced audit-webhook-receiver by more generic tool ([486e04a](https://github.com/ConfigButler/apiservice-audit-proxy/commit/486e04ad25d4ae6e9b07e44873d8af7365233ed2))


### Bug Fixes

* getting rid of warnings by updating ([6321797](https://github.com/ConfigButler/apiservice-audit-proxy/commit/63217972d688d2b4f73071e34f31e47b7efd6da4))

## [0.2.1](https://github.com/ConfigButler/apiservice-audit-proxy/compare/apiservice-audit-proxy-v0.2.0...apiservice-audit-proxy-v0.2.1) (2026-04-22)


### Bug Fixes

* env context is not permitted in jobs ([6585398](https://github.com/ConfigButler/apiservice-audit-proxy/commit/6585398cb1b137e8d6f1b83612aae74058c35df4))

## [0.2.0](https://github.com/ConfigButler/apiservice-audit-proxy/compare/apiservice-audit-proxy-v0.1.1...apiservice-audit-proxy-v0.2.0) (2026-04-22)


### Features

* simple README.md ([916867e](https://github.com/ConfigButler/apiservice-audit-proxy/commit/916867e82b8d852a9b23e35bd685cf37b8eab26c))


### Documentation

* what kind of bug is this of GH?! ([2653903](https://github.com/ConfigButler/apiservice-audit-proxy/commit/26539038cf1c665fcada6185ba34fe27c68c77c1))

## [0.1.1](https://github.com/ConfigButler/apiservice-audit-proxy/compare/apiservice-audit-proxy-v0.1.0...apiservice-audit-proxy-v0.1.1) (2026-04-22)


### Documentation

* Let's clean the mess ([1e39da0](https://github.com/ConfigButler/apiservice-audit-proxy/commit/1e39da0c1b93c9aaf2b3b139011c3f8782aacdec))
