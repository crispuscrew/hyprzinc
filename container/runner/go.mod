module github.com/crispuscrew/zinc/container/runner

go 1.24.2

toolchain go1.24.13

require (
	github.com/crispuscrew/zinc/common v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	golang.org/x/sys v0.27.0 // indirect
)

replace github.com/crispuscrew/zinc/common => ../../common
