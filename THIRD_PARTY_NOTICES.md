# Third-party notices

Chengguang Gateway is distributed under the MIT license in `LICENSE`.
Third-party components retain their own copyrights and licenses.

## Components included in the executable

The executable embeds the compiled web interface and its font assets.
Dependency versions are recorded in `go.mod`, `go.sum` and
`web/package-lock.json`.

| Component | License | Project |
| --- | --- | --- |
| Go runtime and standard library | BSD-3-Clause | [Go](https://go.dev/) |
| `golang.org/x/crypto`, `golang.org/x/sys` | BSD-3-Clause | [Go sub-repositories](https://go.googlesource.com/) |
| Vue and its `@vue/*` runtime packages | MIT | [Vue](https://github.com/vuejs/core) |
| Inter | SIL Open Font License 1.1 | [Inter](https://github.com/rsms/inter) |
| JetBrains Mono | SIL Open Font License 1.1 | [JetBrains Mono](https://github.com/JetBrains/JetBrainsMono) |

The complete font licenses are included in the source tree at
`web/public/licenses/inter-OFL.txt` and
`web/public/licenses/jetbrains-mono-OFL.txt`. Release archives include these as
`Inter-OFL.txt` and `JetBrainsMono-OFL.txt`, alongside this file and `LICENSE`.
The embedded interface also serves them under `/licenses/`.

### Go, golang.org/x/crypto and golang.org/x/sys — BSD-3-Clause

```text
Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

### Vue and @vue runtime packages — MIT

```text
The MIT License (MIT)

Copyright (c) 2018-present, Yuxi (Evan) You

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
```

## Build tools and external services

Node.js, npm, Vite, TypeScript, vue-tsc and other development dependencies are
build tools rather than executable release assets. Their license files are
provided with their respective packages.

Nginx, njs, EasyTier and certd are installed or operated separately; this
project does not redistribute their executables. Cloudflare, Tencent Cloud,
Alibaba Cloud and Globalping integrations do not grant rights to those
services or their trademarks. Their own licenses and service terms apply.
