package main

import (
	"log"
	"os"
	"strings"

	"github.com/NebulousLabs/Sia/build"
	"github.com/lukechampine/us/renter"

	"github.com/lukechampine/flagg"
)

var (
	// to be supplied at build time
	version   = "?"
	githash   = "?"
	goversion = "?"
	builddate = "?"
)

var (
	rootUsage = `Usage:
    user [flags] [action]

Actions:
    scan            scan a host
    form            form a contract
    renew           renew a contract
    upload          upload a file
    download        download a file
    checkup         check the health of a file
    migrate         migrate a file to different hosts
    info            display info about a contract or file
    recover         try to repair a corrupted metafile
`
	versionUsage = rootUsage
	scanUsage    = `Usage:
    user scan
    user scan hostkey bytes duration downloads

Scans the specified host and reports various metrics.

bytes is the number of bytes intended to be stored on the host; duration is
the number of blocks that the contract will be active; downloads is the
expected ratio of downloads to uploads, i.e. downloads = 0.5 means the user
expects to download half of the uploaded data.

A bare 'user scan' will scan and rank all known hosts according to their
latency and prices.
`
	formUsage = `Usage:
    user form hostkey funds endheight [filename]

Forms a contract with the specified host for the specified duration with the
specified amount of funds. Due to various fees, the total number of coins
deducted from the wallet may be greater than funds. Run 'user scan' on the host
to see a breakdown of these fees.

If filename is provided, the resulting contract file will be written to
filename. Otherwise, it will be written to the default contracts directory.
`
	renewUsage = `Usage:
    user renew contract funds endheight [filename]

Renews the specified contract (that is, a .contract file) for the specified
duration and with the specified amount of funds. Due to various fees, the
total number of coins deducted from the wallet may be greater than funds. Run
'user scan' on the host to see a breakdown of these fees.

If filename is provided, the resulting contract file will be written to
filename. Otherwise, it will be written to the default contracts directory.

The old contract file is archived by renaming it to contract_old. In most
cases, these archived contracts can be safely deleted. However, it is prudent
to first verify (with the checkup command) that the new contract is usable.
`
	uploadUsage = `Usage:
    user upload file
    user upload file metafile
    user upload file folder
    user upload folder metafolder

Uploads the specified file or folder, storing its metadata in the specified
metafile or as multiple metafiles within the metafolder. The structure of the
metafolder will mirror that of the folder.

If the first argument is a single file and the second is a folder, the
metafile will be stored within folder, using the filename file.usa. For
example, 'user upload foo.txt bar/' will create the metafile 'bar/foo.txt.usa'.

If the destination is unspecified, it is assumed to be the current directory.
For example, 'user upload foo.txt' will create the metafile 'foo.txt.usa'.
`
	downloadUsage = `Usage:
    user download metafile
    user download metafile file
    user download metafile folder
    user download metafolder folder

Downloads the specified metafile or metafolder, storing file data in the
specified file or as multiple files within the folder. The structure of the
folder will mirror that of the metafolder.

If the first argument is a single metafile and the second is a folder, the
file data will be stored within the folder. This form requires that the
metafile have a .usa extension. The destination filename will be the metafile
without the .usa extension. For example, 'user download foo.txt.usa bar/' will
download to 'bar/foo.txt'.

If the destination is unspecified, it is assumed to be the current directory.
For example, 'user download foo.txt.usa' will download to 'foo.txt'.

However, if the destination file is unspecified and stdout is redirected (e.g.
via a pipe), the downloaded file will be written to stdout. For example,
'user download foo.txt.usa | cat' will display the file in the terminal.
`
	checkupUsage = `Usage:
    user checkup metafile
    user checkup contract

Verifies that a randomly-selected sector of the specified metafile or contract
is retrievable, and reports the resulting metrics for each host. Note that
this operation is not free.
`
	migrateUsage = `Usage:
    user migrate metafile
    user migrate metafolder

Migrates sector data from the metafile's current set of hosts to a new set.
There are three migration strategies, specified by mutually-exclusive flags.
`
	mFileUsage = `Erasure-encode the original file on disk. This is the fastest and
	cheapest option, but it requires a local copy of the file.`

	mDirectUsage = `Upload sectors downloaded directly from old hosts. This is faster and
	cheaper than -remote, but it requires that the old hosts be online.`

	mRemoteUsage = `Download the file from existing hosts and erasure-encode it. This is
	the slowest and most expensive option, but it doesn't require a local
	copy of the file, and it can be performed even if the "old" hosts are
	offline.`

	infoUsage = `Usage:
    user info contract
    user info metafile

Displays information about the specified contract or metafile.
`
	recoverUsage = `Usage:
    user recover metafile

Attempt to recover a metafile after a crash. Use this if you notice a
directory with a _workdir suffix -- this indicates unclean shutdown.
`
	serveUsage = `Usage:
    user serve metafolder

Serve the files in metafolder over HTTP.
`
	mountUsage = `Usage:
    user mount metafolder folder

Mount metafolder as a read-only FUSE filesystem, rooted at folder.
`
)

var usage = flagg.SimpleUsage(flagg.Root, rootUsage) // point-free style!

func check(ctx string, err error) {
	if err != nil {
		log.Fatalln(ctx, err)
	}
}

func main() {
	log.SetFlags(0)

	err := loadConfig()
	if err != nil {
		check("Could not load config file:", err)
	}

	rootCmd := flagg.Root
	rootCmd.StringVar(&config.SiadAddr, "a", config.SiadAddr, "host:port that the siad API is running on")
	rootCmd.StringVar(&config.SiadPassword, "p", config.SiadPassword, "password required by siad API")
	rootCmd.StringVar(&config.Contracts, "c", config.Contracts, "directory where contracts are stored")
	rootCmd.Usage = flagg.SimpleUsage(rootCmd, rootUsage)

	versionCmd := flagg.New("version", versionUsage)
	scanCmd := flagg.New("scan", scanUsage)
	scanN := scanCmd.Int("n", 5, "number of scan results to display")
	formCmd := flagg.New("form", formUsage)
	renewCmd := flagg.New("renew", renewUsage)
	uploadCmd := flagg.New("upload", uploadUsage)
	uploadCmd.IntVar(&config.MinShards, "m", config.MinShards, "minimum number of shards required to download file")
	downloadCmd := flagg.New("download", downloadUsage)
	checkupCmd := flagg.New("checkup", checkupUsage)
	migrateCmd := flagg.New("migrate", migrateUsage)
	mFile := migrateCmd.String("file", "", mFileUsage)
	mDirect := migrateCmd.Bool("direct", false, mDirectUsage)
	mRemote := migrateCmd.Bool("remote", false, mRemoteUsage)
	infoCmd := flagg.New("info", infoUsage)
	recoverCmd := flagg.New("recover", recoverUsage)
	serveCmd := flagg.New("serve", serveUsage)
	sAddr := serveCmd.String("addr", ":8080", "HTTP service address")
	mountCmd := flagg.New("mount", mountUsage)

	cmd := flagg.Parse(flagg.Tree{
		Cmd: rootCmd,
		Sub: []flagg.Tree{
			{Cmd: versionCmd},
			{Cmd: scanCmd},
			{Cmd: formCmd},
			{Cmd: renewCmd},
			{Cmd: uploadCmd},
			{Cmd: downloadCmd},
			{Cmd: checkupCmd},
			{Cmd: migrateCmd},
			{Cmd: infoCmd},
			{Cmd: recoverCmd},
			{Cmd: serveCmd},
			{Cmd: mountCmd},
		},
	})
	args := cmd.Args()

	switch cmd {
	case rootCmd:
		if len(args) > 0 {
			usage()
			return
		}
		fallthrough
	case versionCmd:
		goversion = strings.TrimPrefix(goversion, "go version ")
		log.Printf("user v%s\nCommit:     %s\nRelease:    %s\nGo version: %s\nBuild Date: %s\n",
			version, githash, build.Release, goversion, builddate)

	case scanCmd:
		if len(args) == 0 {
			scanAll(*scanN)
			return
		}
		hostkey, bytes, duration, downloads := parseScan(args, scanCmd)
		err := scan(hostkey, bytes, duration, downloads)
		check("Scan failed:", err)

	case formCmd:
		host, funds, endHeight, filename := parseForm(args, formCmd)
		err := form(host, funds, endHeight, filename)
		check("Contract formation failed:", err)

	case renewCmd:
		contract, funds, endHeight, filename := parseRenew(args, renewCmd)
		err := renew(contract, funds, endHeight, filename)
		check("Renew failed:", err)

	case uploadCmd:
		if config.MinShards == 0 {
			log.Fatalln(`Upload failed: minimum number of shards not specified.
Define min_shards in your config file or supply the -m flag.`)
		}
		f, meta := parseUpload(args, uploadCmd)
		var err error
		if stat, statErr := f.Stat(); statErr == nil && stat.IsDir() {
			err = uploadmetadir(f.Name(), meta, config.Contracts, config.MinShards)
		} else if _, statErr := os.Stat(meta); !os.IsNotExist(statErr) {
			err = resumeuploadmetafile(f, config.Contracts, meta)
		} else {
			err = uploadmetafile(f, config.MinShards, config.Contracts, meta)
		}
		f.Close()
		check("Upload failed:", err)

	case downloadCmd:
		f, meta := parseDownload(args, downloadCmd)
		var err error
		if stat, statErr := f.Stat(); statErr == nil && stat.IsDir() {
			err = downloadmetadir(f.Name(), config.Contracts, meta)
		} else if f == os.Stdout {
			err = downloadmetastream(f, config.Contracts, meta)
		} else {
			err = downloadmetafile(f, config.Contracts, meta)
			f.Close()
		}
		check("Download failed:", err)

	case checkupCmd:
		path := parseCheckup(args, checkupCmd)
		var err error
		if _, readErr := renter.ReadMetaIndex(path); readErr == nil {
			err = checkupMeta(config.Contracts, path)
		} else if _, readErr := renter.ReadContractTransaction(path); readErr == nil {
			err = checkupContract(path)
		} else {
			log.Fatalln("Not a valid contract or meta file")
		}
		check("Checkup failed:", err)

	case migrateCmd:
		if len(args) == 0 {
			migrateCmd.Usage()
			return
		}
		meta := args[0]
		stat, statErr := os.Stat(meta)
		isDir := statErr == nil && stat.IsDir()
		var err error
		switch {
		case *mFile == "" && !*mDirect && !*mRemote:
			log.Fatalln("No migration strategy specified (see user migrate --help).")
		case *mFile != "" && !isDir:
			f, ferr := os.Open(*mFile)
			check("Could not open file:", ferr)
			err = migrateFile(f, config.Contracts, meta)
			f.Close()
		case *mFile != "" && isDir:
			err = migrateDirFile(*mFile, config.Contracts, meta)
		case *mDirect && !isDir:
			err = migrateDirect(config.Contracts, meta)
		case *mDirect && isDir:
			err = migrateDirDirect(config.Contracts, meta)
		case *mRemote && !isDir:
			err = migrateRemote(config.Contracts, meta)
		case *mRemote && isDir:
			err = migrateDirRemote(config.Contracts, meta)
		default:
			log.Fatalln("Multiple migration strategies specified (see user migrate --help).")
		}
		check("Migration failed:", err)

	case infoCmd:
		if len(args) != 1 {
			infoCmd.Usage()
			return
		}

		if index, shards, err := renter.ReadMetaFileContents(args[0]); err == nil {
			metainfo(index, shards)
		} else if h, err := renter.ReadContractTransaction(args[0]); err == nil {
			contractinfo(h)
		} else {
			log.Fatalln("Not a contract or meta file")
		}

	case recoverCmd:
		if len(args) != 1 {
			recoverCmd.Usage()
			return
		}
		err := recoverMeta(args[0])
		check("Recovery failed:", err)

	case serveCmd:
		if len(args) != 1 {
			serveCmd.Usage()
			return
		}
		err := serve(config.Contracts, args[0], *sAddr)
		if err != nil {
			log.Fatal(err)
		}

	case mountCmd:
		if len(args) != 2 {
			mountCmd.Usage()
			return
		}
		err := mount(config.Contracts, args[0], args[1])
		if err != nil {
			log.Fatal(err)
		}
	}
}
