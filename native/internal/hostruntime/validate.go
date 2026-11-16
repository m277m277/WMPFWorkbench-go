package hostruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Fixed RVAs in the embedded agents apply only to these measured binaries.
var runtimeHashes = map[string]string{
	"runtime/flue.dll":              "a66d9388ff0b6fbd79045551e9d5b22dadc460ebd1b5659733361921617e22f6",
	"host/wmpf_host_export_x64.dll": "d4ee4ebd7066bf1fca47e1b7003f21d6e240afd5b5209432938bcf86f38f6025",
	"runtime/libamssdk.dll":         "80e80064578b6cc155ffdcfd67520f5013c71d37611876fe1768dfa59f4b4807",
	"runtime/pcyyb_sdk_dll.dll":     "426bee2cd0ffb26d44e1e2d852b5545d6ac8a9c5a682db779975ad13d2f11e2d",
	"runtime/WeChatAppEx.exe":       "2a927250e538ed6194e0136fc36e92cb727d5008ac1953d7a7a41364e20c15d3",
}

func ValidateRuntime(ctx context.Context, root string) error {
	for name, want := range runtimeHashes {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return errors.New("Runtime 文件缺失或不可读取: " + name)
		}
		h := sha256.New()
		buf := make([]byte, 1<<20)
		for {
			if err = ctx.Err(); err != nil {
				break
			}
			var n int
			n, err = f.Read(buf)
			if n > 0 {
				h.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		f.Close()
		if err != nil && err != io.EOF {
			return errors.New("Runtime 校验失败: " + name)
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			return errors.New("Runtime 版本或文件校验不匹配，禁止使用固定 RVA: " + name)
		}
	}
	return nil
}
