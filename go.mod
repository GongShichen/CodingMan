module github.com/GongShichen/CodingMan

go 1.26.0

require github.com/GongShichen/CodingMan/agent v0.0.0

require github.com/GongShichen/CodingMan/tool v0.0.0 // indirect

require (
	github.com/anthropics/anthropic-sdk-go v1.37.0 // indirect
	github.com/openai/openai-go/v3 v3.31.0 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	golang.org/x/sync v0.20.0 // indirect
)

replace github.com/GongShichen/CodingMan/agent => ./agent

replace github.com/GongShichen/CodingMan/tool => ./tool
