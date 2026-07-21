// Package config 解析 config.yml,提供全局配置 Conf。
// 当前配置:groups(Group 列表,启动时批量建)+ etcd endpoints。
package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是整个配置文件的根结构。
type Config struct {
	Groups []GroupConfig `yaml:"groups"` // 缓存 Group 列表,启动时按此批量建
	Etcd   EtcdConfig    `yaml:"etcd"`   // etcd 集群配置
}

// GroupConfig 是单个缓存 Group 的配置。
type GroupConfig struct {
	Name         string `yaml:"name"`         // Group 名(命名空间,如 scores/users)
	EvictionType string `yaml:"evictionType"` // 淘汰策略:lru/fifo/lfu
	MaxBytes     int64  `yaml:"maxBytes"`     // 内存上限(字节),0=不限
	TTL          string `yaml:"ttl"`          // 全局 TTL,如 "10m";空或 0=不启用
}

// TTLDuration 把字符串 TTL("10m")解析成 time.Duration;空/解析失败返回 0(不启用 TTL)。
func (c GroupConfig) TTLDuration() time.Duration {
	if c.TTL == "" || c.TTL == "0" {
		return 0
	}
	d, err := time.ParseDuration(c.TTL)
	if err != nil {
		return 0 // 解析失败静默不启用,不 panic(配置错误容忍)
	}
	return d
}

// EtcdConfig 是 etcd 集群配置。
type EtcdConfig struct {
	Endpoints []string `yaml:"endpoints"` // etcd 节点地址列表,client/v3 自动故障转移
}

// Conf 是全局配置,Init 后填充。
var Conf Config

// Init 从 path 读 yaml 配置文件,解析到全局 Conf。
func Init(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, &Conf)
}
