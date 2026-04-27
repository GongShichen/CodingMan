module github.com/GongShichen/CodingMan/agent

go 1.26

require (
	github.com/GongShichen/CodingMan/tool v0.0.0
	github.com/anthropics/anthropic-sdk-go v1.37.0
	github.com/openai/openai-go/v3 v3.31.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	golang.org/x/sync v0.20.0 // indirect
)

replace github.com/GongShichen/CodingMan/tool => ../tool
