package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/jarvisgally/v2simple/common"
	"github.com/jarvisgally/v2simple/proxy"
	"github.com/jarvisgally/v2simple/proxy/direct"
	_ "github.com/jarvisgally/v2simple/proxy/socks5"
	_ "github.com/jarvisgally/v2simple/proxy/tls"
	_ "github.com/jarvisgally/v2simple/proxy/vmess"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

var (
	// Version
	version  = "0.1.0"
	codename = "V2Simple, a simple implementation of V2Ray 4.25.0"

	// Flag
	f = flag.String("f", "client.example.json", "config file name")
)

const (
	// 路由模式
	whitelist = "whitelist"
	blacklist = "blacklist"
)

//
// Version
//

func printVersion() {
	fmt.Printf("V2Simple %v (%v), %v %v %v\n", version, codename, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

//
// Config
//

type Config struct {
	Local  string `json:"local"`
	Route  string `json:"route"`
	Remote string `json:"remote"`
	FilePath string `json:"filePath"`
	OutFilePath string `json:"outFilePath"`
}

func loadConfig(configFileName string) (*Config, error) {
	path := common.GetPath(configFileName)
	if len(path) > 0 {
		if cf, err := os.Open(path); err == nil {
			defer cf.Close()
			bytes, _ := ioutil.ReadAll(cf)
			config := &Config{}
			if err = json.Unmarshal(bytes, config); err != nil {
				return nil, fmt.Errorf("can not parse config file %v, %v", configFileName, err)
			}
			return config, nil
		}
	}
	return nil, fmt.Errorf("can not load config file %v", configFileName)
}

func check(e error) {
	if e != nil {
		panic(e)
	}
}

func main() {
	// 打印版本信息
	printVersion()

	// 解析命令行参数
	flag.Parse()

	// 读取配置文件，默认为客户端模式
	conf, err := loadConfig(*f)
	if err != nil {
		log.Printf("can not load config file: %v", err)
		os.Exit(-1)
	}

	// 根据配置文件初始化组件
	localServer, err := proxy.ServerFromURL(conf.Local)
	if err != nil {
		log.Printf("can not create local server: %v", err)
		os.Exit(-1)
	}
	defer localServer.Stop() // Server可能有一些定时任务，使用Stop关闭
	remoteClient, err := proxy.ClientFromURL(conf.Remote)
	if err != nil {
		log.Printf("can not create remote client: %v", err)
		os.Exit(-1)
	}
	//directClient, _ := proxy.ClientFromURL("direct://")
	//matcher := common.NewMather(conf.Route)

	// 开启本地的TCP监听
	listener, err := net.Listen("tcp", localServer.Addr())
	if err != nil {
		log.Printf("can not listen on %v: %v", localServer.Addr(), err)
		os.Exit(-1)
	}
	log.Printf("%v listening TCP on %v", localServer.Name(), localServer.Addr())
	go func() {
		for {
			lc, err := listener.Accept()
			if err != nil {
				errStr := err.Error()
				if strings.Contains(errStr, "closed") {
					break
				}
				log.Printf("failed to accepted connection: %v", err)
				if strings.Contains(errStr, "too many") {
					time.Sleep(time.Millisecond * 500)
				}
				continue
			}
			go func() {
				defer lc.Close()
				var client proxy.Client

				// 不同的服务端协议各自实现自己的响应逻辑, 其中返回的地址则用于匹配路由
				// 常常需要额外编解码或者流量统计的功能，故需要给lc包一层以实现这些逻辑，即wlc
				wlc, targetAddr, err := localServer.Handshake(lc)
				if err != nil {
					log.Printf("failed in handshake from %v: %v", localServer.Addr(), err)
					return
				}

				// 匹配路由
				//if conf.Route == whitelist { // 白名单模式，如果匹配，则直接访问，否则使用代理访问
				//	if matcher.Check(targetAddr.Host()) {
				//		client = directClient
				//	} else {
				//		client = remoteClient
				//	}
				//} else if conf.Route == blacklist { // 黑名单模式，如果匹配，则使用代理访问，否则直接访问
				//	if matcher.Check(targetAddr.Host()) {
				//		client = remoteClient
				//	} else {
				//		client = directClient
				//	}
				//} else { // 全部流量使用代理访问
				//	client = remoteClient
				//}
				if conf.OutFilePath != "" {
					client = remoteClient
					log.Printf("%v to %v", client.Name(), targetAddr)

					// 连接远端地址
					dialAddr := remoteClient.Addr()
					if _, ok := client.(*direct.Direct); ok { // 直接访问则直接连接目标地址
						dialAddr = targetAddr.String()
					}
					rc, err := net.Dial("tcp", dialAddr)
					if err != nil {
						log.Printf("failed to dail to %v: %v", dialAddr, err)
						return
					}
					defer rc.Close()

					// 不同的客户端协议各自实现自己的请求逻辑
					wrc, err := client.Handshake(rc, targetAddr.String())
					if err != nil {
						log.Printf("failed in handshake to %v: %v", dialAddr, err)
						return
					}

					if _, err := os.Stat(conf.OutFilePath); err == nil {
						if err := os.Remove(conf.OutFilePath); err != nil {
							log.Fatal(err)
							return
						}
					}

					f, err := os.OpenFile(conf.OutFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
					check(err)
					defer f.Close()

					// 流量转发
					go io.Copy(wrc, wlc)
					io.Copy(f, wrc)
					return
				}
				_, err = os.ReadFile(conf.FilePath)
				check(err)
				f, err := os.Open(conf.FilePath)
				check(err)
				defer f.Close()
				buf := make([]byte, 32 * 1024)
				now := time.Now().UnixMilli()
				send := 0
				count := 0
				for {
					nr, er := f.Read(buf)
					if nr > 0 {
						nw, ew := wlc.Write(buf[0:nr])
						if nw < 0 || nr < nw {
							nw = 0
						}
						check(ew)
						if nr != nw {
							log.Printf("error in writing")
							break
						}
						send += nw
						count++
						diff := time.Now().UnixMilli() - now
						if count % 1000 == 0 {
							log.Printf("send %v bytes, %v ms", send, diff)
						}
					}
					if er != nil {
						break
					}
				}

			}()
		}
	}()

	// 后台运行
	{
		osSignals := make(chan os.Signal, 1)
		signal.Notify(osSignals, os.Interrupt, os.Kill, syscall.SIGTERM)
		<-osSignals
	}
}
