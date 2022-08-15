package bufio

import (
	"context"
	"io"
	"net"
	"os"

	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/rw"
	"github.com/sagernet/sing/common/task"
)

type readOnlyReader struct {
	io.Reader
}

func (r *readOnlyReader) WriteTo(w io.Writer) (n int64, err error) {
	return Copy(w, r.Reader)
}

func needReadFromWrapper(dst io.ReaderFrom, src io.Reader) bool {
	_, isTCPConn := dst.(*net.TCPConn)
	if !isTCPConn {
		return false
	}
	switch src.(type) {
	case *net.TCPConn, *net.UnixConn, *os.File:
		return false
	default:
		return true
	}
}

func Copy(dst io.Writer, src io.Reader) (n int64, err error) {
	if src == nil {
		return 0, E.New("nil reader")
	} else if dst == nil {
		return 0, E.New("nil writer")
	}
	src = N.UnwrapReader(src)
	dst = N.UnwrapWriter(dst)
	if wt, ok := src.(io.WriterTo); ok {
		return wt.WriteTo(dst)
	}
	if rt, ok := dst.(io.ReaderFrom); ok {
		if needReadFromWrapper(rt, src) {
			src = &readOnlyReader{src}
		}
		return rt.ReadFrom(src)
	}
	return CopyExtended(NewExtendedWriter(dst), NewExtendedReader(src))
}

func CopyExtended(dst N.ExtendedWriter, src N.ExtendedReader) (n int64, err error) {
	unsafeSrc, srcUnsafe := common.Cast[N.ThreadSafeReader](src)
	headroom := N.CalculateFrontHeadroom(dst) + N.CalculateRearHeadroom(dst)
	if srcUnsafe {
		if headroom == 0 {
			return CopyExtendedWithSrcBuffer(dst, unsafeSrc)
		}
	}
	if N.IsUnsafeWriter(dst) {
		return CopyExtendedWithPool(dst, src)
	}
	bufferSize := N.CalculateMTU(src, dst)
	if bufferSize > 0 {
		bufferSize += headroom
	} else {
		bufferSize = buf.BufferSize
	}
	_buffer := buf.StackNewSize(bufferSize)
	defer common.KeepAlive(_buffer)
	buffer := common.Dup(_buffer)
	defer buffer.Release()
	return CopyExtendedBuffer(dst, src, buffer)
}

func CopyExtendedBuffer(dst N.ExtendedWriter, src N.ExtendedReader, buffer *buf.Buffer) (n int64, err error) {
	buffer.IncRef()
	defer buffer.DecRef()
	frontHeadroom := N.CalculateFrontHeadroom(dst)
	rearHeadroom := N.CalculateRearHeadroom(dst)
	readBufferRaw := buffer.Slice()
	readBuffer := buf.With(readBufferRaw[:cap(readBufferRaw)-rearHeadroom])
	var notFirstTime bool
	for {
		readBuffer.Resize(frontHeadroom, 0)
		err = src.ReadBuffer(readBuffer)
		if err != nil {
			if !notFirstTime {
				err = N.HandshakeFailure(dst, err)
			}
			return
		}
		dataLen := readBuffer.Len()
		buffer.Resize(readBuffer.Start(), dataLen)
		err = dst.WriteBuffer(buffer)
		if err != nil {
			return
		}
		n += int64(dataLen)
		notFirstTime = true
	}
}

func CopyExtendedWithSrcBuffer(dst N.ExtendedWriter, src N.ThreadSafeReader) (n int64, err error) {
	var notFirstTime bool
	for {
		var buffer *buf.Buffer
		buffer, err = src.ReadBufferThreadSafe()
		if err != nil {
			if !notFirstTime {
				err = N.HandshakeFailure(dst, err)
			}
			return
		}
		dataLen := buffer.Len()
		err = dst.WriteBuffer(buffer)
		if err != nil {
			buffer.Release()
			return
		}
		n += int64(dataLen)
		notFirstTime = true
	}
}

func CopyExtendedWithPool(dst N.ExtendedWriter, src N.ExtendedReader) (n int64, err error) {
	frontHeadroom := N.CalculateFrontHeadroom(dst)
	rearHeadroom := N.CalculateRearHeadroom(dst)
	bufferSize := N.CalculateMTU(src, dst)
	if bufferSize > 0 {
		bufferSize += frontHeadroom + rearHeadroom
	} else {
		bufferSize = buf.BufferSize
	}
	var notFirstTime bool
	for {
		buffer := buf.NewSize(bufferSize)
		readBufferRaw := buffer.Slice()
		readBuffer := buf.With(readBufferRaw[:cap(readBufferRaw)-rearHeadroom])
		readBuffer.Resize(frontHeadroom, 0)
		err = src.ReadBuffer(readBuffer)
		if err != nil {
			buffer.Release()
			if !notFirstTime {
				err = N.HandshakeFailure(dst, err)
			}
			return
		}
		dataLen := readBuffer.Len()
		buffer.Resize(readBuffer.Start(), dataLen)
		err = dst.WriteBuffer(buffer)
		if err != nil {
			buffer.Release()
			return
		}
		n += int64(dataLen)
		notFirstTime = true
	}
}

func CopyConn(ctx context.Context, conn net.Conn, dest net.Conn) error {
	var group task.Group
	group.Append("upload", func(ctx context.Context) error {
		defer rw.CloseWrite(dest)
		return common.Error(Copy(dest, conn))
	})
	if _, srcDuplex := common.Cast[rw.WriteCloser](conn); srcDuplex {
		group.Append("download", func(ctx context.Context) error {
			defer rw.CloseWrite(conn)
			return common.Error(Copy(conn, dest))
		})
	} else {
		group.Append("download", func(ctx context.Context) error {
			defer common.Close(conn)
			return common.Error(Copy(conn, dest))
		})
	}
	group.Cleanup(func() {
		common.Close(conn, dest)
	})
	return group.Run(ctx)
}

func CopyPacket(dst N.PacketWriter, src N.PacketReader) (n int64, err error) {
	unsafeSrc, srcUnsafe := common.Cast[N.ThreadSafePacketReader](src)
	frontHeadroom := N.CalculateFrontHeadroom(dst)
	rearHeadroom := N.CalculateRearHeadroom(dst)
	headroom := frontHeadroom + rearHeadroom
	if srcUnsafe {
		if headroom == 0 {
			return CopyPacketWithSrcBuffer(dst, unsafeSrc)
		}
	}
	if N.IsUnsafeWriter(dst) {
		return CopyPacketWithPool(dst, src)
	}
	bufferSize := N.CalculateMTU(src, dst)
	if bufferSize > 0 {
		bufferSize += headroom
	} else {
		bufferSize = buf.UDPBufferSize
	}
	_buffer := buf.StackNewSize(bufferSize)
	defer common.KeepAlive(_buffer)
	buffer := common.Dup(_buffer)
	defer buffer.Release()
	buffer.IncRef()
	defer buffer.DecRef()
	var destination M.Socksaddr
	var notFirstTime bool
	readBufferRaw := buffer.Slice()
	readBuffer := buf.With(readBufferRaw[:cap(readBufferRaw)-rearHeadroom])
	for {
		readBuffer.Resize(frontHeadroom, 0)
		destination, err = src.ReadPacket(readBuffer)
		if err != nil {
			if !notFirstTime {
				err = N.HandshakeFailure(dst, err)
			}
			return
		}
		dataLen := readBuffer.Len()
		buffer.Resize(readBuffer.Start(), dataLen)
		err = dst.WritePacket(buffer, destination)
		if err != nil {
			return
		}
		n += int64(dataLen)
		notFirstTime = true
	}
}

func CopyPacketWithSrcBuffer(dst N.PacketWriter, src N.ThreadSafePacketReader) (n int64, err error) {
	var buffer *buf.Buffer
	var destination M.Socksaddr
	var notFirstTime bool
	for {
		buffer, destination, err = src.ReadPacketThreadSafe()
		if err != nil {
			if !notFirstTime {
				err = N.HandshakeFailure(dst, err)
			}
			return
		}
		dataLen := buffer.Len()
		err = dst.WritePacket(buffer, destination)
		if err != nil {
			buffer.Release()
			return
		}
		n += int64(dataLen)
		notFirstTime = true
	}
}

func CopyPacketWithPool(dst N.PacketWriter, src N.PacketReader) (n int64, err error) {
	frontHeadroom := N.CalculateFrontHeadroom(dst)
	rearHeadroom := N.CalculateRearHeadroom(dst)
	bufferSize := N.CalculateMTU(src, dst)
	if bufferSize > 0 {
		bufferSize += frontHeadroom + rearHeadroom
	} else {
		bufferSize = buf.UDPBufferSize
	}
	var destination M.Socksaddr
	var notFirstTime bool
	for {
		buffer := buf.NewSize(bufferSize)
		readBufferRaw := buffer.Slice()
		readBuffer := buf.With(readBufferRaw[:cap(readBufferRaw)-rearHeadroom])
		readBuffer.Resize(frontHeadroom, 0)
		destination, err = src.ReadPacket(readBuffer)
		if err != nil {
			buffer.Release()
			if !notFirstTime {
				err = N.HandshakeFailure(dst, err)
			}
			return
		}
		dataLen := readBuffer.Len()
		buffer.Resize(readBuffer.Start(), dataLen)
		err = dst.WritePacket(buffer, destination)
		if err != nil {
			buffer.Release()
			return
		}
		n += int64(dataLen)
		notFirstTime = true
	}
}

func CopyPacketConn(ctx context.Context, conn N.PacketConn, dest N.PacketConn) error {
	var group task.Group
	group.Append("upload", func(ctx context.Context) error {
		return common.Error(CopyPacket(dest, conn))
	})
	group.Append("download", func(ctx context.Context) error {
		return common.Error(CopyPacket(conn, dest))
	})
	group.Cleanup(func() {
		common.Close(conn, dest)
	})
	group.FastFail()
	return group.Run(ctx)
}
