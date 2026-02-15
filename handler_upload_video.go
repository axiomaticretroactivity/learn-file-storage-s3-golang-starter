package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	const maxMemory = 1 << 30
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to parse file", err)
		return
	}

	videoMetadata, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}

	if videoMetadata.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Video does not belong to current user", err)
		return
	}

	video, videoHeaderPtr, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to form file", err)
		return
	}
	defer video.Close()

	videoMediaType := videoHeaderPtr.Header.Get("Content-Type")
	parsedType, _, err := mime.ParseMediaType(videoMediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Missing or malformed Content-Type header", err)
		return
	}
	if parsedType != "video/mp4" {
		respondWithError(w, http.StatusUnsupportedMediaType, "Videos must be in mp4 format", fmt.Errorf("bad upload received (video not mp4)"))
		return
	}

	rawVideoFile, err := os.CreateTemp("", "tubely-upload-*.mp4") // create the raw video from upload
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create file", err)
		return
	}
	defer rawVideoFile.Close()
	defer os.Remove(rawVideoFile.Name())

	_, err = io.Copy(rawVideoFile, video)
	if err != nil {
		respondWithError(w, http.StatusInsufficientStorage, "Could not write new file", err)
		return
	}

	err = rawVideoFile.Close() // need manual close here before ffmpeg can deal with it
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to process video", err)
		return
	}

	videoNewFilePath, err := processVideoForFastStart(rawVideoFile.Name()) // process the video
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to process video", err)
		return
	}

	videoNewFile, err := os.Open(videoNewFilePath) // open the processed video and continue
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to process video", err)
		return
	}
	defer os.Remove(videoNewFile.Name())
	defer videoNewFile.Close()

	//_, err = videoNewFile.Seek(0, io.SeekStart)
	//if err != nil {
	//	respondWithError(w, http.StatusInternalServerError, "Could not reset new file pointer", err)
	//	return
	//}

	videoMediaExts, err := mime.ExtensionsByType(videoMediaType)
	if err != nil || len(videoMediaExts) == 0 {
		respondWithError(w, http.StatusInternalServerError, "Failed to get video extension", err)
		return
	}
	videoMediaExt := videoMediaExts[0]

	urlRandBytes := make([]byte, 32)
	_, err = rand.Read(urlRandBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to generate URL for video", err)
	}
	urlString := hex.EncodeToString(urlRandBytes)

	aspectRatio, err := getVideoAspectRatio(videoNewFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to get video aspect ratio", err)
		return
	}

	var ratioPrefix string
	if aspectRatio == "16:9" {
		ratioPrefix = "/landscape/"
	} else if aspectRatio == "9:16" {
		ratioPrefix = "/portrait/"
	} else {
		ratioPrefix = "/other/"
	}

	videoName := ratioPrefix + urlString + videoMediaExt

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &videoName,
		Body:        videoNewFile,
		ContentType: &videoMediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not upload video to AWS", err)
		return
	}

	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, videoName)

	videoMetadata.VideoURL = &videoURL
	videoMetadata.UpdatedAt = time.Now()

	err = cfg.db.UpdateVideo(videoMetadata)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoMetadata)
}

func getVideoAspectRatio(filePath string) (string, error) {
	type ffprobeOutput struct {
		Streams []struct {
			Index              int    `json:"index"`
			CodecName          string `json:"codec_name,omitempty"`
			CodecLongName      string `json:"codec_long_name,omitempty"`
			Profile            string `json:"profile,omitempty"`
			CodecType          string `json:"codec_type"`
			CodecTagString     string `json:"codec_tag_string"`
			CodecTag           string `json:"codec_tag"`
			Width              int    `json:"width,omitempty"`
			Height             int    `json:"height,omitempty"`
			CodedWidth         int    `json:"coded_width,omitempty"`
			CodedHeight        int    `json:"coded_height,omitempty"`
			ClosedCaptions     int    `json:"closed_captions,omitempty"`
			FilmGrain          int    `json:"film_grain,omitempty"`
			HasBFrames         int    `json:"has_b_frames,omitempty"`
			SampleAspectRatio  string `json:"sample_aspect_ratio,omitempty"`
			DisplayAspectRatio string `json:"display_aspect_ratio,omitempty"`
			PixFmt             string `json:"pix_fmt,omitempty"`
			Level              int    `json:"level,omitempty"`
			ColorRange         string `json:"color_range,omitempty"`
			ColorSpace         string `json:"color_space,omitempty"`
			ColorTransfer      string `json:"color_transfer,omitempty"`
			ColorPrimaries     string `json:"color_primaries,omitempty"`
			ChromaLocation     string `json:"chroma_location,omitempty"`
			FieldOrder         string `json:"field_order,omitempty"`
			Refs               int    `json:"refs,omitempty"`
			IsAvc              string `json:"is_avc,omitempty"`
			NalLengthSize      string `json:"nal_length_size,omitempty"`
			ID                 string `json:"id"`
			RFrameRate         string `json:"r_frame_rate"`
			AvgFrameRate       string `json:"avg_frame_rate"`
			TimeBase           string `json:"time_base"`
			StartPts           int    `json:"start_pts"`
			StartTime          string `json:"start_time"`
			DurationTs         int    `json:"duration_ts"`
			Duration           string `json:"duration"`
			BitRate            string `json:"bit_rate,omitempty"`
			BitsPerRawSample   string `json:"bits_per_raw_sample,omitempty"`
			NbFrames           string `json:"nb_frames"`
			ExtradataSize      int    `json:"extradata_size"`
			Disposition        struct {
				Default         int `json:"default"`
				Dub             int `json:"dub"`
				Original        int `json:"original"`
				Comment         int `json:"comment"`
				Lyrics          int `json:"lyrics"`
				Karaoke         int `json:"karaoke"`
				Forced          int `json:"forced"`
				HearingImpaired int `json:"hearing_impaired"`
				VisualImpaired  int `json:"visual_impaired"`
				CleanEffects    int `json:"clean_effects"`
				AttachedPic     int `json:"attached_pic"`
				TimedThumbnails int `json:"timed_thumbnails"`
				NonDiegetic     int `json:"non_diegetic"`
				Captions        int `json:"captions"`
				Descriptions    int `json:"descriptions"`
				Metadata        int `json:"metadata"`
				Dependent       int `json:"dependent"`
				StillImage      int `json:"still_image"`
			} `json:"disposition"`
			Tags struct {
				Language    string `json:"language"`
				HandlerName string `json:"handler_name"`
				VendorID    string `json:"vendor_id"`
				Encoder     string `json:"encoder"`
				Timecode    string `json:"timecode"`
			} `json:"tags,omitempty"`
			SampleFmt      string `json:"sample_fmt,omitempty"`
			SampleRate     string `json:"sample_rate,omitempty"`
			Channels       int    `json:"channels,omitempty"`
			ChannelLayout  string `json:"channel_layout,omitempty"`
			BitsPerSample  int    `json:"bits_per_sample,omitempty"`
			InitialPadding int    `json:"initial_padding,omitempty"`
			Tags0          struct {
				Language    string `json:"language"`
				HandlerName string `json:"handler_name"`
				VendorID    string `json:"vendor_id"`
			} `json:"tags,omitempty"`
			Tags1 struct {
				Language    string `json:"language"`
				HandlerName string `json:"handler_name"`
				Timecode    string `json:"timecode"`
			} `json:"tags,omitempty"`
		} `json:"streams"`
	}

	command := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var buf bytes.Buffer
	command.Stdout = &buf
	err := command.Run()
	if err != nil {
		return "", err
	}

	var videoData ffprobeOutput
	err = json.Unmarshal(buf.Bytes(), &videoData)
	if err != nil {
		return "", err
	}
	if len(videoData.Streams) == 0 {
		return "", fmt.Errorf("no video data found")
	}

	width := videoData.Streams[0].Width
	height := videoData.Streams[0].Height
	aspectRatio := float64(width) / float64(height)
	tolerance := 0.03
	if math.Abs(aspectRatio-16.0/9.0) < tolerance {
		return "16:9", nil
	} else if math.Abs(aspectRatio-9.0/16.0) < tolerance {
		return "9:16", nil
	}
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outputFilePath := filePath + ".processing"
	command := exec.Command(
		"ffmpeg",
		"-i",
		filePath,
		"-c",
		"copy",
		"-movflags",
		"faststart",
		"-f",
		"mp4",
		outputFilePath,
	)
	err := command.Run()
	if err != nil {
		return "", err
	}

	return outputFilePath, nil
}
