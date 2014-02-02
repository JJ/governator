package main

import (
	"bytes"
	"code.google.com/p/go.exp/fsnotify"
	"errors"
	"fmt"
	"gnd.la/log"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
)

var services struct {
	sync.Mutex
	list []*Service
}

type servicesByPriority []*Service

func (s servicesByPriority) Len() int           { return len(s) }
func (s servicesByPriority) Less(i, j int) bool { return s[i].Config.Priority < s[j].Config.Priority }
func (s servicesByPriority) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s servicesByPriority) Sort()              { sort.Stable(s) }

type quit struct {
	stop    chan bool
	stopped chan bool
}

func newQuit() *quit {
	return &quit{
		stop:    make(chan bool, 1),
		stopped: make(chan bool, 1),
	}
}

func (q *quit) sendStop() {
	q.stop <- true
}

func (q *quit) sendStopped() {
	q.stopped <- true
}

func startWatching(q *quit) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	go func() {
	End:
		for {
			select {
			case ev := <-watcher.Event:
				name := filepath.Base(ev.Name)
				if shouldIgnoreFile(name) {
					break
				}
				services.Lock()
				switch {
				case ev.IsCreate():
					cfg := ParseConfig(name)
					log.Debugf("added service %s", cfg.ServiceName())
					s := newService(cfg)
					services.list = append(services.list, s)
					servicesByPriority(services.list).Sort()
					s.Start()
				case ev.IsDelete() || ev.IsRename():
					for ii := range services.list {
						s := services.list[ii]
						if s.Config.File == name {
							log.Debugf("removed service %s", s.Name())
							if s.State == StateStarted {
								s.Stop()
							}
							services.list = append(services.list[:ii], services.list[ii+1:]...)
							break
						}
					}
				case ev.IsModify():
					for _, v := range services.list {
						if v.Config.File == name {
							cfg := ParseConfig(name)
							if reflect.DeepEqual(v.Config, cfg) {
								// there were changes to the file which don't affect the conf
								break
							}
							log.Debugf("changed service %s's configuration", v.Name())
							start := false
							if v.State == StateStarted {
								start = v.Stop() == nil
							}
							v.Config = cfg
							servicesByPriority(services.list).Sort()
							if start {
								v.Start()
							}
							break
						}
					}
				default:
					log.Errorf("unhandled event: %s\n", ev)
				}
				services.Unlock()
			case err := <-watcher.Error:
				log.Errorf("error watching: %s", err)
			case <-q.stop:
				watcher.Close()
				q.sendStopped()
				break End
			}
		}
	}()
	if err := watcher.Watch(*configDir); err != nil {
		return err
	}
	return nil
}

func startService(conn net.Conn, s *Service) error {
	name := s.Name()
	encodeResponse(conn, respOk, fmt.Sprintf("starting %s\n", name))
	if serr := s.Start(); serr != nil {
		return encodeResponse(conn, respErr, fmt.Sprintf("error starting %s: %s\n", name, serr))
	}
	return encodeResponse(conn, respOk, fmt.Sprintf("started %s\n", name))
}

func stopService(conn net.Conn, s *Service) (bool, error) {
	name := s.Name()
	encodeResponse(conn, respOk, fmt.Sprintf("stopping %s\n", name))
	if serr := s.Stop(); serr != nil {
		return false, encodeResponse(conn, respErr, fmt.Sprintf("error stopping %s: %s\n", name, serr))
	}
	return true, encodeResponse(conn, respOk, fmt.Sprintf("stopped %s\n", name))
}

func serveConn(conn net.Conn) error {
	defer conn.Close()
	args, err := decodeArgs(conn)
	if err != nil {
		return fmt.Errorf("error decoding arguments: %s", err)
	}
	if len(args) > 0 {
		var err error
		var st *Service
		var name string
		cmd := strings.ToLower(args[0])
		if cmd == "start" || cmd == "stop" || cmd == "restart" || cmd == "log" {
			if len(args) != 2 {
				err = encodeResponse(conn, respErr, fmt.Sprintf("command %s requires exactly one argument\n", cmd))
				cmd = ""
			}
			if cmd != "" {
				services.Lock()
				for _, v := range services.list {
					if sn := v.Name(); sn == args[1] {
						st = v
						name = sn
						break
					}
				}
				services.Unlock()
				if st == nil {
					err = encodeResponse(conn, respErr, fmt.Sprintf("no service named %s\n", args[1]))
					cmd = ""
				}
			}
		}
		switch cmd {
		case "":
			// cmd already handled
		case "start":
			if st.State == StateStarted {
				err = encodeResponse(conn, respErr, fmt.Sprintf("%s is already running\n", name))
			} else {
				err = startService(conn, st)
			}
		case "stop":
			if st.State != StateStarted {
				err = encodeResponse(conn, respErr, fmt.Sprintf("%s is not running\n", name))
			} else {
				_, err = stopService(conn, st)
			}
		case "restart":
			var stopped bool
			if st.State == StateStarted {
				stopped, err = stopService(conn, st)
			}
			if stopped {
				err = startService(conn, st)
			}
		case "list":
			var buf bytes.Buffer
			w := tabwriter.NewWriter(&buf, 4, 4, 4, ' ', 0)
			fmt.Fprint(w, "SERVICE\tSTATUS\t\n")
			services.Lock()
			for _, v := range services.list {
				fmt.Fprintf(w, "%s\t", v.Name())
				switch v.State {
				case StateStopped:
					fmt.Fprint(w, "STOPPED")
				case StateStopping:
					fmt.Fprint(w, "STOPPING")
				case StateStarting:
					fmt.Fprint(w, "STARTING")
				case StateStarted:
					if v.Restarts > 0 {
						fmt.Fprintf(w, "RUNNING since %s - %d restarts", formatTime(v.Started), v.Restarts)
					} else {
						fmt.Fprintf(w, "RUNNING since %s", formatTime(v.Started))
					}
				case StateFailed:
					fmt.Fprintf(w, "FAILED - %s", v.Err)
				default:
					panic("invalid state")
				}
				fmt.Fprint(w, "\t\n")
			}
			services.Unlock()
			w.Flush()
			buf.WriteString("\n")
			err = encodeResponse(conn, respOk, buf.String())
		case "log":
			if st.State != StateStarted {
				err = encodeResponse(conn, respErr, fmt.Sprintf("%s is not running\n", name))
				break
			}
			if st.logger.monitor != nil {
				err = encodeResponse(conn, respErr, fmt.Sprintf("%s is already being monitored\n", name))
				break
			}
			ch := make(chan bool, 1)
			st.logger.monitor = func(prefix string, b []byte) {
				var buf bytes.Buffer
				buf.WriteByte('[')
				buf.WriteString(prefix)
				buf.WriteString("] ")
				buf.Write(b)
				if b[len(b)-1] != '\n' {
					buf.Write(newLine)
				}
				encodeResponse(conn, respOk, buf.String())
			}
			go func() {
				// log stops when the client sends something over the connection
				// or the connection is closed
				b := make([]byte, 1)
				conn.Read(b)
				conn.Close()
				ch <- true
			}()
			<-ch
			st.logger.monitor = nil
			return nil
		default:
			err = encodeResponse(conn, respErr, fmt.Sprintf("unknown command %s - %s\n", cmd, help))
			if err != nil {
				return err
			}
		}
	}
	return encodeResponse(conn, respEnd, "")
}

func startServer(q *quit) error {
	os.Remove(SocketPath)
	server, err := net.Listen("unix", SocketPath)
	if err != nil {
		return err
	}
	if gid := getGroupId(AppName); gid >= 0 {
		os.Chown(SocketPath, 0, gid)
		os.Chmod(SocketPath, 0775)
	}
	conns := make(chan net.Conn, 10)
	go func() {
		for {
			conn, err := server.Accept()
			if err != nil {
				log.Errorf("error accepting connection: %s", err)
			}
			conns <- conn
		}
	}()
	go func() {
		for {
			select {
			case <-q.stop:
				os.Remove(SocketPath)
				q.sendStopped()
				return
			case conn := <-conns:
				go func() {
					if err := serveConn(conn); err != nil {
						log.Errorf("error serving connection: %s", err)
					}
				}()
			}
		}
	}()
	return nil
}

func daemonMain() error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	if u.Uid != "0" {
		return errors.New("govenator daemon must be run as root")
	}
	// Make logs directory
	os.Mkdir(LogDir, 0755)
	configs, err := ParseConfigs()
	if err != nil {
		return err
	}
	services.Lock()
	services.list = make([]*Service, len(configs))
	for ii, v := range configs {
		s := newService(v)
		services.list[ii] = s
		s.Start()
	}
	servicesByPriority(services.list).Sort()
	services.Unlock()
	quitWatcher := newQuit()
	if err := startWatching(quitWatcher); err != nil {
		log.Errorf("error watching %s, configuration won't be automatically updated: %s", *configDir, err)
	}
	quitServer := newQuit()
	if err := startServer(quitServer); err != nil {
		log.Errorf("error starting server, can't receive remote commands: %s", err)
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)
	// Wait for signal
	<-c
	quitWatcher.sendStop()
	quitServer.sendStop()
	// Wait for goroutines to exit cleanly
	<-quitWatcher.stopped
	<-quitServer.stopped
	services.Lock()
	var wg sync.WaitGroup
	wg.Add(len(services.list))
	// Stop in reverse order, to respect priorities
	for ii := len(services.list) - 1; ii >= 0; ii-- {
		v := services.list[ii]
		go func(s *Service) {
			s.Stop()
			wg.Done()
		}(v)
	}
	wg.Wait()
	services.Unlock()
	log.Debugf("daemon exiting")
	return nil
}
