module github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend

go 1.26.0

require (
	github.com/MicahParks/keyfunc/v3 v3.8.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/segmentio/kafka-go v0.4.51
	github.com/wso2-open-operations/cs-tools/apps/chat-routing-service/sdk-go v0.0.0-00010101000000-000000000000
	golang.org/x/oauth2 v0.27.0
)

// The routing-service Go SDK is a nested module in this same monorepo
// (apps/chat-routing-service/sdk-go) with no published version yet -- see
// that module's README.md "Versioning" section. Once it's tagged, drop
// this replace and pin a real version instead.
replace github.com/wso2-open-operations/cs-tools/apps/chat-routing-service/sdk-go => ../../chat-routing-service/sdk-go

require (
	github.com/MicahParks/jwkset v0.11.0 // indirect
	github.com/klauspost/compress v1.15.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
	golang.org/x/time v0.9.0 // indirect
)
