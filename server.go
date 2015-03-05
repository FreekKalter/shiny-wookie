package main

import (
	"container/list"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Queue struct {
	current string
	M       sync.Mutex
	list.List
}

var (
	pause, exit chan bool
	threads     string
)

func main() {
	// init
	threads = "2"
	// Setup signal handeling
	exit = make(chan bool, 1)
	pause = make(chan bool, 1)
	signalChan := make(chan os.Signal, 2)
	signal.Notify(signalChan, os.Interrupt)
	go func() {
		for {
			<-signalChan
			handle_exit()
		}
	}()

	// start goroutine handeling emptying the queue
	q := &Queue{}
	go compress(q, exit)

	// setting up everthing needed for the tcp connection
	ln, err := net.Listen("tcp", ":1234")
	if err != nil {
		log.Fatal("[*] could not liston on port 1234:", err)
	}
	defer ln.Close()
	fmt.Println("[-] Listening for connections on port 1234")
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Print("error on accepting connection: ", err)
		}
		go handle_conn(conn, q)
	}
}

func handle_exit() {
	select {
	case exit <- true:
		fmt.Println("[-] shutting down gracefully, waiting for currently converted file to finish")
	default: // already send 1 interupt for graceful shutdown, (so exit chan will block)force it a second time
		fmt.Println("[+] shutting down forcefully, after receiving second request")
		os.Exit(0)
	}
}

func handle_conn(c net.Conn, q *Queue) {
	defer c.Close()
	buff, err := ioutil.ReadAll(c)
	if err != nil {
		log.Print("error on reading from connection:", err)
	}
	filenames := strings.Split(strings.Trim(string(buff), "\n "), "\n")
	if len(filenames) == 1 && strings.HasPrefix(filenames[0], "--") {
		stripped := strings.TrimLeft(filenames[0], "--")
		command := strings.Split(stripped, " ")
		switch {
		case command[0] == "list":
			c.Write([]byte(fmt.Sprintf("working on: %s\n", q.current)))
			for e := q.Front(); e != nil; e = e.Next() {
				c.Write([]byte(fmt.Sprintf("%s\n", e.Value.(string))))
			}
		case command[0] == "pause":
			fmt.Println("[-] got pause message from client")
			c.Write([]byte("received pause command\n"))
			pause <- true
		case command[0] == "resume":
			fmt.Println("[+] got resume message from client")
			c.Write([]byte("received resume command\n"))
			pause <- false
		case command[0] == "stop":
			handle_exit()
		case command[0] == "threads":
			if _, err := strconv.ParseInt(command[1], 10, 32); err == nil {
				threads = command[1]
				fmt.Printf("[+] setting number of threads to %s\n", threads)
				c.Write([]byte(fmt.Sprintf("setting number of threads to %s\n", threads)))
			} else {
				c.Write([]byte("[*] number of threads must be an integer\n"))
			}
		}
		return
	}
	q.M.Lock()
	for _, f := range filenames {
		dup := false
		for e := q.Front(); e != nil; e = e.Next() {
			if e.Value == f || f == q.current {
				dup = true
				break
			}
		}
		if !dup {
			q.PushBack(f)
			c.Write([]byte(fmt.Sprintf("added to queue: %s\n", f)))
		} else {
			c.Write([]byte(fmt.Sprintf("already in queue: %s\n", f)))
		}
	}
	q.M.Unlock()
}

func compress(q *Queue, exit chan bool) {
	for {
		select {
		case <-exit:
			os.Exit(0)
		default:
			if q.Len() > 0 {
				q.M.Lock()
				item := q.Remove(q.Front())
				filename := item.(string)
				q.current = filename
				q.M.Unlock()
				if _, err := os.Stat(filename); os.IsNotExist(err) {
					fmt.Printf("%s does not exist (anymore), skipping\n", filename)
					continue
				}
				isoregex := regexp.MustCompile("(?i:iso|img)$")
				var err error
				if isoregex.MatchString(filename) {
					err = convertIso(filename)
				} else {
					err = convertVideo(filename)
				}
				if err != nil {
					fmt.Println(err)
				} else {
					err := os.Remove(filename)
					if err != nil {
						fmt.Println(prefixError("failed to remove "+filename, err))
					} else {
						fmt.Printf("[+] %s compressed and old one deleted\n", filename)
					}
				}
			}
		}
		// if queue is empty, the neverending for loop wil run amok
		time.Sleep(1 * time.Second)
	}
}

func prefixError(prefix string, err error) error {
	return errors.New(fmt.Sprintf("[*] %s: %s", prefix, err))
}

func findBestResolution(filename string) string {
	res := "1280x720"
	cmd := exec.Command("ffmpeg", "-i", filename)
	out, _ := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, ": Video:") {
			re := regexp.MustCompile("([0-9]{2,5})x[0-9]{2,5}")
			horizontal, _ := strconv.ParseInt(re.FindStringSubmatch(line)[1], 10, 64)
			if horizontal < 1280 {
				res = re.FindStringSubmatch(line)[0]
			}
		}
	}
	return res
}

func convertIso(filename string) error {
	mountPoint := "/media/film"
	cmd := exec.Command("sudo", "umount", mountPoint)
	err := cmd.Run()
	cmd = exec.Command("sudo", "mount", filename, mountPoint)
	err = cmd.Run()
	if err != nil {
		return prefixError("error mounting %s: %s", err)
	}

	var input, resolution string
	if _, err := os.Stat(filepath.Join(mountPoint, "VIDEO_TS")); !os.IsNotExist(err) {
		vobs, err := filepath.Glob(filepath.Join(mountPoint, "VIDEO_TS", "VTS_01_[1-9].VOB"))
		if err != nil {
			return prefixError("globbing:", err)
		}
		input = "concat:"
		for i, f := range vobs {
			if i == len(vobs)-1 {
				input = fmt.Sprintf("%s%s", input, f)
			} else {
				input = fmt.Sprintf("%s%s|", input, f)
			}
		}
		resolution = findBestResolution("/media/film/VIDEO_TS/VTS_01_1.VOB")
	} else if _, err := os.Stat(filepath.Join(mountPoint, "BDMW")); !os.IsNotExist(err) {
		media, _ := filepath.Glob(filepath.Join(mountPoint, "BDWM", "*"))
		var size int64
		for _, f := range media {
			f = filepath.Join(mountPoint, "BDWM", f)
			if stat, err := os.Stat(f); err == nil {
				if s := stat.Size(); s > size {
					size, input = s, f
				}
			}
		}
		resolution = findBestResolution(input)
	}
	newfile := filepath.Join(filepath.Dir(filename), "compressed.mp4")
	cmd = exec.Command("ffmpeg", "-i", input,
		"-sn",                             // disable subtitles
		"-c:v", "libx264", "-vf", "yadif", // x264 video codec, video filter to deinterlace video
		"-crf", "27", // constant rate factor, compromise between quality and size
		"-s", resolution, // set output resolution
		"-c:a", "copy", // just copy the audio, no de/encoding
		"-threads", threads, "-y", newfile) // 2 threads to throttle cpu usage, -y to overwrite output file
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Start()
	if err != nil {
		return prefixError("compressing: ", err)
	}
	err = process_wait(cmd)
	if err != nil {
		return err
	}

	cmd = exec.Command("sudo", "umount", "/media/film")
	err = cmd.Run()
	if err != nil {
		return prefixError("unmounting: ", err)
	}
	err = os.Chown(newfile, 1000, 1000) // uid of fkalter
	if err != nil {
		return prefixError("chowning: ", err)
	}
	return nil
}

func process_wait(cmd *exec.Cmd) error {
	done := make(chan error)
	fmt.Println("[-] starting: ", cmd.Path, cmd.Args)
	go func() {
		done <- cmd.Wait()
	}()
selectloop:
	for {
		select {
		case p := <-pause:
			if p {
				cmd.Process.Signal(syscall.SIGSTOP)
			} else {
				cmd.Process.Signal(syscall.SIGCONT)
			}
		case err := <-done:
			if err != nil {
				return prefixError(cmd.Path+" return code:", err)
			}
			fmt.Println("[+] completed: ", cmd.Path, cmd.Args)
			break selectloop
		default:
			time.Sleep(2 * time.Second)
		}
	}
	return nil
}

func convertVideo(filename string) error {
	resolution := findBestResolution(filename)
	newfile := filepath.Join(filepath.Dir(filename), fmt.Sprintf("%s-compressed.mp4",
		strings.Replace(filepath.Base(filename), filepath.Ext(filename), "", -1)))

	cmd := exec.Command("ffmpeg", "-i", filename,
		"-sn",                             // disable subtitles
		"-c:v", "libx264", "-vf", "yadif", // x264 video codec, video filter to deinterlace video
		"-crf", "27", // constant rate factor, compromise between quality and size
		"-s", resolution, // set output resolution
		"-c:a", "copy", // just copy the audio, no de/encoding
		"-threads", threads, "-y", newfile) // 2 threads to throttle cpu usage, -y to overwrite output file
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Start()
	if err != nil {
		return prefixError("compressing: ", err)
	}
	err = process_wait(cmd)
	if err != nil {
		return err
	}
	err = os.Chown(newfile, 1000, 1000) // uid of fkalter
	if err != nil {
		return prefixError("chowning: ", err)
	}
	return nil
}
