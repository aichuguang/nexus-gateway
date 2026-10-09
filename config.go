package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"w103d/gateway/internal/app"
	"w103d/gateway/internal/engine"
)

type startupConfig struct {
	GatewayIP  string `json:"gatewayIp"`
	Port       int    `json:"port"`
	InitialKey string `json:"initialKey"`
}

func readStartupConfig(path string) (startupConfig, error) {
	var config startupConfig
	info, err := os.Stat(path)
	if err != nil {
		return config, fmt.Errorf("无法读取配置文件 %s：%w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return config, errors.New("config.json 必须是小于 64 KiB 的普通 JSON 文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("无法读取配置文件 %s：%w", path, err)
	}
	if len(data) > 64<<10 || !utf8.Valid(data) {
		return config, errors.New("config.json 必须使用 UTF-8 编码，且不超过 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, errors.New("config.json 格式不正确；仅支持 gatewayIp、port、initialKey 三项")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return config, errors.New("config.json 只能包含一个 JSON 对象")
	}
	return config, nil
}

func applyStartupConfig(options *app.Options, flags *flag.FlagSet, executable, workingDirectory string) error {
	explicit := map[string]bool{}
	flags.Visit(func(value *flag.Flag) { explicit[value.Name] = true })
	// Existing systemd units already supply their gateway explicitly. Preserve
	// that deployment contract unless the operator explicitly selects a file.
	if explicit["gateway-ip"] && !explicit["config"] {
		return nil
	}
	path := ""
	if explicit["config"] {
		path = flags.Lookup("config").Value.String()
		if path == "" {
			return errors.New("--config 需要指定配置文件路径")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workingDirectory, path)
		}
	} else {
		path = filepath.Join(filepath.Dir(executable), "config.json")
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			path = filepath.Join(workingDirectory, "config.json")
		}
	}
	config, err := readStartupConfig(path)
	if err != nil {
		if !explicit["config"] && errors.Is(err, os.ErrNotExist) {
			return errors.New("未找到 config.json；请在程序同目录填写 gatewayIp、port 和 initialKey 后启动")
		}
		return err
	}
	if !explicit["gateway-ip"] {
		options.GatewayIP = strings.TrimSpace(config.GatewayIP)
	}
	if !engine.ValidVPNIPv4(options.GatewayIP) {
		return errors.New("请在 config.json 的 gatewayIp 填写盒子实际分配的 EasyTier IPv4 地址；不限定网段")
	}
	if !explicit["listen"] {
		if config.Port < 1 || config.Port > 65535 {
			return errors.New("config.json 的 port 必须是 1 到 65535 之间的整数")
		}
		if config.Port == engine.DomainEntryPort || config.Port == engine.HTTPRedirectPort || config.Port == engine.GenerationPort {
			return errors.New("config.json 的 port 不能占用代理保留端口 80、443 或 18079")
		}
		options.Listen = net.JoinHostPort(options.GatewayIP, strconv.Itoa(config.Port))
	}
	options.InitialKey = config.InitialKey
	options.RequireInitialKey = true
	return nil
}
