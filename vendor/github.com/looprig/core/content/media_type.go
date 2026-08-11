package content

// MediaType is the IANA media type (MIME type) for block content.
// Named constants below cover the types accepted by current AI providers;
// callers may construct other values for provider-specific or future types.
type MediaType string

// Image MIME types accepted by multimodal providers.
const (
	MediaTypeImageJPEG MediaType = "image/jpeg"    // .jpg / .jpeg
	MediaTypeImagePNG  MediaType = "image/png"     // .png
	MediaTypeImageGIF  MediaType = "image/gif"     // .gif
	MediaTypeImageWebP MediaType = "image/webp"    // .webp
	MediaTypeImageSVG  MediaType = "image/svg+xml" // .svg
)

// Audio MIME types.
const (
	MediaTypeAudioMPEG MediaType = "audio/mpeg" // .mp3
	MediaTypeAudioWAV  MediaType = "audio/wav"  // .wav
	MediaTypeAudioOGG  MediaType = "audio/ogg"  // .ogg
	MediaTypeAudioFLAC MediaType = "audio/flac" // .flac
	MediaTypeAudioAAC  MediaType = "audio/aac"  // .aac
	MediaTypeAudioMP4  MediaType = "audio/mp4"  // .m4a
	MediaTypeAudioWebM MediaType = "audio/webm" // .webm
)

// Document MIME types.
const (
	MediaTypeDocumentPDF      MediaType = "application/pdf" // .pdf
	MediaTypeDocumentText     MediaType = "text/plain"      // .txt
	MediaTypeDocumentHTML     MediaType = "text/html"       // .html
	MediaTypeDocumentCSV      MediaType = "text/csv"        // .csv
	MediaTypeDocumentMarkdown MediaType = "text/markdown"   // .md
	// Office open XML formats — the modern .docx/.xlsx wire types.
	MediaTypeDocumentDOCX MediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MediaTypeDocumentXLSX MediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)
