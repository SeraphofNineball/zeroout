// Package carve finds files in raw free space by recognising their headers
// at block boundaries (ext4 always starts file data on a block boundary) and
// working out where each one ends.
package carve

const (
	KB = 1 << 10
	MB = 1 << 20
	GB = 1 << 30
)

// Kind is a recognisable file type.
type Kind struct {
	Ext      string `json:"ext"`
	Category string `json:"category"`
	Desc     string `json:"desc"`
	MIME     string `json:"mime"`
	// container formats can hold other files at block boundaries, so headers
	// inside them don't start a new file.
	container bool
	maxSize   int64
}

const (
	CatImages    = "Images"
	CatVideo     = "Video"
	CatAudio     = "Audio"
	CatDocuments = "Documents"
	CatArchives  = "Archives"
	CatDatabases = "Databases"
	CatPrograms  = "Programs"
	CatText      = "Text"
)

// Kinds lists every type in a fixed order; Item.Kind indexes it. Scans store
// this table, so append new kinds at the end.
var Kinds = []Kind{
	{"jpg", CatImages, "JPEG image", "image/jpeg", false, 256 * MB},
	{"png", CatImages, "PNG image", "image/png", false, 256 * MB},
	{"gif", CatImages, "GIF image", "image/gif", false, 128 * MB},
	{"bmp", CatImages, "Bitmap image", "image/bmp", false, 512 * MB},
	{"webp", CatImages, "WebP image", "image/webp", false, 128 * MB},
	{"tif", CatImages, "TIFF image", "image/tiff", false, GB},
	{"heic", CatImages, "HEIC/HEIF image", "image/heic", false, 256 * MB},
	{"avif", CatImages, "AVIF image", "image/avif", false, 256 * MB},
	{"psd", CatImages, "Photoshop document", "image/vnd.adobe.photoshop", false, 4 * GB},
	{"cr2", CatImages, "Canon RAW (CR2)", "image/x-canon-cr2", false, 512 * MB},
	{"cr3", CatImages, "Canon RAW (CR3)", "image/x-canon-cr3", false, 512 * MB},
	{"nef", CatImages, "Nikon RAW", "image/x-nikon-nef", false, 512 * MB},
	{"arw", CatImages, "Sony RAW", "image/x-sony-arw", false, 512 * MB},
	{"orf", CatImages, "Olympus RAW", "image/x-olympus-orf", false, 512 * MB},
	{"rw2", CatImages, "Panasonic RAW", "image/x-panasonic-rw2", false, 512 * MB},
	{"pef", CatImages, "Pentax RAW", "image/x-pentax-pef", false, 512 * MB},

	{"mp4", CatVideo, "MPEG-4 video", "video/mp4", false, 64 * GB},
	{"mov", CatVideo, "QuickTime video", "video/quicktime", false, 64 * GB},
	{"m4v", CatVideo, "iTunes video", "video/x-m4v", false, 64 * GB},
	{"3gp", CatVideo, "3GPP video", "video/3gpp", false, 8 * GB},
	{"mkv", CatVideo, "Matroska video", "video/x-matroska", false, 64 * GB},
	{"webm", CatVideo, "WebM video", "video/webm", false, 64 * GB},
	{"avi", CatVideo, "AVI video", "video/x-msvideo", false, 16 * GB},
	{"wmv", CatVideo, "Windows Media video", "video/x-ms-wmv", false, 16 * GB},
	{"flv", CatVideo, "Flash video", "video/x-flv", false, 8 * GB},
	{"mpg", CatVideo, "MPEG program stream", "video/mpeg", false, 32 * GB},
	{"ts", CatVideo, "MPEG transport stream", "video/mp2t", false, 64 * GB},
	{"ogv", CatVideo, "Ogg video", "video/ogg", false, 8 * GB},

	{"mp3", CatAudio, "MP3 audio", "audio/mpeg", false, GB},
	{"flac", CatAudio, "FLAC audio", "audio/flac", false, 4 * GB},
	{"wav", CatAudio, "WAV audio", "audio/wav", false, 4 * GB},
	{"aiff", CatAudio, "AIFF audio", "audio/aiff", false, 4 * GB},
	{"m4a", CatAudio, "MPEG-4 audio", "audio/mp4", false, 4 * GB},
	{"ogg", CatAudio, "Ogg audio", "audio/ogg", false, 2 * GB},
	{"opus", CatAudio, "Opus audio", "audio/opus", false, 2 * GB},
	{"wma", CatAudio, "Windows Media audio", "audio/x-ms-wma", false, 2 * GB},
	{"mid", CatAudio, "MIDI", "audio/midi", false, 16 * MB},

	{"pdf", CatDocuments, "PDF document", "application/pdf", true, 2 * GB},
	{"docx", CatDocuments, "Word document", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", true, GB},
	{"xlsx", CatDocuments, "Excel workbook", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", true, GB},
	{"pptx", CatDocuments, "PowerPoint presentation", "application/vnd.openxmlformats-officedocument.presentationml.presentation", true, 4 * GB},
	{"odt", CatDocuments, "OpenDocument text", "application/vnd.oasis.opendocument.text", true, GB},
	{"ods", CatDocuments, "OpenDocument spreadsheet", "application/vnd.oasis.opendocument.spreadsheet", true, GB},
	{"odp", CatDocuments, "OpenDocument presentation", "application/vnd.oasis.opendocument.presentation", true, GB},
	{"epub", CatDocuments, "EPUB e-book", "application/epub+zip", true, GB},
	{"doc", CatDocuments, "Word 97-2003 document", "application/msword", true, GB},
	{"xls", CatDocuments, "Excel 97-2003 workbook", "application/vnd.ms-excel", true, GB},
	{"ppt", CatDocuments, "PowerPoint 97-2003", "application/vnd.ms-powerpoint", true, 2 * GB},
	{"msg", CatDocuments, "Outlook message", "application/vnd.ms-outlook", true, GB},
	{"ole", CatDocuments, "Other Office/OLE file", "application/x-ole-storage", true, 2 * GB},
	{"rtf", CatDocuments, "Rich text document", "application/rtf", false, 512 * MB},

	{"zip", CatArchives, "ZIP archive", "application/zip", true, 64 * GB},
	{"jar", CatArchives, "Java archive", "application/java-archive", true, GB},
	{"apk", CatArchives, "Android package", "application/vnd.android.package-archive", true, 4 * GB},
	{"rar", CatArchives, "RAR archive", "application/vnd.rar", true, 64 * GB},
	{"7z", CatArchives, "7-Zip archive", "application/x-7z-compressed", true, 64 * GB},
	{"gz", CatArchives, "Gzip archive", "application/gzip", false, 64 * GB},
	{"bz2", CatArchives, "Bzip2 archive", "application/x-bzip2", false, 64 * GB},
	{"xz", CatArchives, "XZ archive", "application/x-xz", false, 64 * GB},
	{"tar", CatArchives, "Tar archive", "application/x-tar", true, 64 * GB},
	{"cab", CatArchives, "Cabinet archive", "application/vnd.ms-cab-compressed", true, 4 * GB},

	{"sqlite", CatDatabases, "SQLite database", "application/vnd.sqlite3", false, 64 * GB},

	{"exe", CatPrograms, "Windows program", "application/vnd.microsoft.portable-executable", false, 4 * GB},
	{"dll", CatPrograms, "Windows library", "application/vnd.microsoft.portable-executable", false, GB},
	{"elf", CatPrograms, "Linux program/library", "application/x-elf", false, 4 * GB},

	{"txt", CatText, "Plain text", "text/plain; charset=utf-8", false, 256 * MB},
	{"html", CatText, "HTML page", "text/plain; charset=utf-8", false, 64 * MB},
	{"xml", CatText, "XML", "text/plain; charset=utf-8", false, 256 * MB},
	{"svg", CatText, "SVG image", "text/plain; charset=utf-8", false, 64 * MB},
	{"json", CatText, "JSON", "text/plain; charset=utf-8", false, 256 * MB},
	{"eml", CatText, "Email message", "text/plain; charset=utf-8", false, 256 * MB},
	{"vcf", CatText, "Contact card", "text/plain; charset=utf-8", false, 16 * MB},
	{"ics", CatText, "Calendar", "text/plain; charset=utf-8", false, 16 * MB},
	{"sh", CatText, "Script", "text/plain; charset=utf-8", false, 16 * MB},
}

var kindIndex = func() map[string]uint16 {
	m := make(map[string]uint16, len(Kinds))
	for i, k := range Kinds {
		m[k.Ext] = uint16(i)
	}
	return m
}()

// KindByExt returns the index of the kind with extension ext.
func KindByExt(ext string) (uint16, bool) {
	i, ok := kindIndex[ext]
	return i, ok
}
