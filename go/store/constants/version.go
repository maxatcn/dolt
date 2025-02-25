// Copyright 2019 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// This file incorporates work covered by the following copyright and
// permission notice:
//
// Copyright 2016 Attic Labs, Inc. All rights reserved.
// Licensed under the Apache License, version 2.0:
// http://www.apache.org/licenses/LICENSE-2.0

// Package constants collects common constants used in Noms, such as the Noms data format version.
package constants

import "os"

func init() {
	nbfVerStr := os.Getenv("DOLT_DEFAULT_BIN_FORMAT")
	if nbfVerStr != "" {
		FormatDefaultString = nbfVerStr
	}
}

var NomsGitSHA = "<developer build>"

// See //go/store/types/format.go for corresponding formats.

const FormatLD1String = "__LD_1__"
const FormatDoltString = "__DOLT__"

var FormatDefaultString = FormatDoltString
