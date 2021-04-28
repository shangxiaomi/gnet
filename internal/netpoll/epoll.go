// Copyright (c) 2019 Andy Pan
// Copyright (c) 2017 Joshua J Baker
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

// +build linux

package netpoll

import (
	"os"
	"runtime"
	"sync/atomic"
	"unsafe"

	"github.com/panjf2000/gnet/errors"
	"github.com/panjf2000/gnet/internal/logging"
	"github.com/panjf2000/gnet/internal/netpoll/queue"
	"golang.org/x/sys/unix"
)

// Poller represents a poller which is in charge of monitoring file-descriptors.
type Poller struct {
	fd             int    // epoll fd
	wfd            int    // wake fd
	wfdBuf         []byte // wfd buffer to read packet
	netpollWakeSig int32
	asyncTaskQueue queue.AsyncTaskQueue
}

// OpenPoller instantiates a poller.
func OpenPoller() (poller *Poller, err error) {
	poller = new(Poller)
	if poller.fd, err = unix.EpollCreate1(unix.EPOLL_CLOEXEC); err != nil {
		poller = nil
		err = os.NewSyscallError("epoll_create1", err)
		return
	}
	if poller.wfd, err = unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC); err != nil {
		_ = poller.Close()
		poller = nil
		err = os.NewSyscallError("eventfd", err)
		return
	}
	poller.wfdBuf = make([]byte, 8)
	if err = poller.AddRead(poller.wfd); err != nil {
		_ = poller.Close()
		poller = nil
		return
	}
	poller.asyncTaskQueue = queue.NewLockFreeQueue()
	return
}

// Close closes the poller.
func (p *Poller) Close() error {
	if err := os.NewSyscallError("close", unix.Close(p.fd)); err != nil {
		return err
	}
	return os.NewSyscallError("close", unix.Close(p.wfd))
}

// Make the endianness of bytes compatible with more linux OSs under different processor-architectures,
// according to http://man7.org/linux/man-pages/man2/eventfd.2.html.
var (
	u uint64 = 1
	b        = (*(*[8]byte)(unsafe.Pointer(&u)))[:]
)

// Trigger wakes up the poller blocked in waiting for network-events and runs jobs in asyncTaskQueue.
func (p *Poller) Trigger(task queue.Task) (err error) {
	// 任务入队
	p.asyncTaskQueue.Enqueue(task)
	if atomic.CompareAndSwapInt32(&p.netpollWakeSig, 0, 1) {
		for _, err = unix.Write(p.wfd, b); err == unix.EINTR || err == unix.EAGAIN; _, err = unix.Write(p.wfd, b) {
		}
	}
	return os.NewSyscallError("write", err)
}

// Polling blocks the current goroutine, waiting for network-events.
func (p *Poller) Polling(callback func(fd int, ev uint32) error) error {
	// 创建时间列表集合
	el := newEventList(InitEvents)
	var wakenUp bool
	// 为什么一开始会返回一个fd
	msec := -1
	for {
		// 在这里监听可以使用的描述符，p.fd是epoll占用的，是el.events存放返回的事件，并进行处理
		// mainLoop中的p.fd就是 监听的epoll的fd
		n, err := unix.EpollWait(p.fd, el.events, msec)
		if n == 0 || (n < 0 && err == unix.EINTR) {
			msec = -1
			runtime.Gosched()
			continue
		} else if err != nil {
			logging.DefaultLogger.Warnf("Error occurs in epoll: %v", os.NewSyscallError("epoll_wait", err))
			return err
		}
		msec = 0

		for i := 0; i < n; i++ {
			// 主进程在这里一定是一直可读
			// TODO 这里的fd会不会包含其他eventloop的wfd ??????
			if fd := int(el.events[i].Fd); fd != p.wfd {
				// 回调函数，当有读事件发生时执行回调函数
				switch err = callback(fd, el.events[i].Events); err {
				case nil:
				case errors.ErrAcceptSocket, errors.ErrServerShutdown:
					return err
				default:
					logging.DefaultLogger.Warnf("Error occurs in event-loop: %v", err)
				}
			} else {
				wakenUp = true
				// 唤醒wfd ?
				// 如果内核计数器为0，就会一直阻塞在这里
				/*
				如果计数器中的值大于0
				- 设置了EFD_SEMAPHORE标志位，则返回1，且计数器中的值也减去1。
				- 没有设置EFD_SEMAPHORE标志位，则返回计数器中的值，且计数器置0。


				如果计数器中的值为0
				- 设置了EFD_NONBLOCK标志位就直接返回-1。
				- 没有设置EFD_NONBLOCK标志位就会一直阻塞直到计数器中的值大于0。

				作者：haozhn
				链接：https://juejin.cn/post/6844903592457928711
				来源：掘金
				著作权归作者所有。商业转载请联系作者获得授权，非商业转载请注明出处。
				 */
				_, _ = unix.Read(p.wfd, p.wfdBuf)
			}
		}

		// 这边是干什么的？用于进程间通信？因为wfd通过 EventFd函数创建？待了解相关实现
		if wakenUp {
			wakenUp = false
			var task queue.Task
			for i := 0; i < AsyncTasks; i++ {
				// 任务出队
				if task = p.asyncTaskQueue.Dequeue(); task == nil {
					break
				}
				switch err = task(); err {
				case nil:
				case errors.ErrServerShutdown:
					return err
				default:
					logging.DefaultLogger.Warnf("Error occurs in user-defined function, %v", err)
				}
			}
			atomic.StoreInt32(&p.netpollWakeSig, 0)
			// 这里怎么解读？
			if !p.asyncTaskQueue.Empty() {
				// 将缓冲区的8字节正兴致加到内核计数器上
				/*
					EAGAIN : Resource temporarily unavailable
					EINTR错误的产生：当阻塞于某个慢系统调用的一个进程捕获某个信号且相应信号处理函数返回时，该系统调用可能返回一个EINTR错误。例如：在socket服务器端，设置了信号捕获机制，有子进程，当在父进程阻塞于慢系统调用时由父进程捕获到了一个有效信号时，内核会致使accept返回一个EINTR错误(被中断的系统调用)。
					————————————————
					版权声明：本文为CSDN博主「风去沙来」的原创文章，遵循CC 4.0 BY-SA版权协议，转载请附上原文出处链接及本声明。
					原文链接：https://blog.csdn.net/yygydjkthh/article/details/7284302
				如果写入值的和小于0xFFFFFFFFFFFFFFFE，则写入成功

				如果写入值的和大于0xFFFFFFFFFFFFFFFE
				- 设置了EFD_NONBLOCK标志位就直接返回-1。
				- 如果没有设置EFD_NONBLOCK标志位，则会一直阻塞知道read操作执行

				作者：haozhn
				链接：https://juejin.cn/post/6844903592457928711
				来源：掘金
				著作权归作者所有。商业转载请联系作者获得授权，非商业转载请注明出处。
				*/
				for _, err = unix.Write(p.wfd, b); err == unix.EINTR || err == unix.EAGAIN; _, err = unix.Write(p.wfd, b) {
				}
			}
		}

		if n == el.size {
			el.expand()
		} else if n < el.size>>1 {
			el.shrink()
		}
	}
}

const (
	readEvents      = unix.EPOLLPRI | unix.EPOLLIN
	writeEvents     = unix.EPOLLOUT
	readWriteEvents = readEvents | writeEvents
)

// AddReadWrite registers the given file-descriptor with readable and writable events to the poller.
func (p *Poller) AddReadWrite(fd int) error {
	return os.NewSyscallError("epoll_ctl add",
		unix.EpollCtl(p.fd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Fd: int32(fd), Events: readWriteEvents}))
}

// AddRead registers the given file-descriptor with readable event to the poller.
func (p *Poller) AddRead(fd int) error {
	return os.NewSyscallError("epoll_ctl add",
		unix.EpollCtl(p.fd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Fd: int32(fd), Events: readEvents}))
}

// AddWrite registers the given file-descriptor with writable event to the poller.
func (p *Poller) AddWrite(fd int) error {
	return os.NewSyscallError("epoll_ctl add",
		unix.EpollCtl(p.fd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Fd: int32(fd), Events: writeEvents}))
}

// ModRead renews the given file-descriptor with readable event in the poller.
func (p *Poller) ModRead(fd int) error {
	return os.NewSyscallError("epoll_ctl mod",
		unix.EpollCtl(p.fd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{Fd: int32(fd), Events: readEvents}))
}

// ModReadWrite renews the given file-descriptor with readable and writable events in the poller.
func (p *Poller) ModReadWrite(fd int) error {
	return os.NewSyscallError("epoll_ctl mod",
		unix.EpollCtl(p.fd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{Fd: int32(fd), Events: readWriteEvents}))
}

// Delete removes the given file-descriptor from the poller.
func (p *Poller) Delete(fd int) error {
	return os.NewSyscallError("epoll_ctl del", unix.EpollCtl(p.fd, unix.EPOLL_CTL_DEL, fd, nil))
}
