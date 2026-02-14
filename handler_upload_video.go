package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
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

	videoNewFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create file", err)
		return
	}
	defer os.Remove(videoNewFile.Name())
	defer videoNewFile.Close()

	_, err = io.Copy(videoNewFile, video)
	if err != nil {
		respondWithError(w, http.StatusInsufficientStorage, "Could not write new file", err)
		return
	}

	_, err = videoNewFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not reset new file pointer", err)
		return
	}

	videoMediaExts, err := mime.ExtensionsByType(videoMediaType)
	if err != nil || len(videoMediaExts) == 0 {
		respondWithError(w, http.StatusInternalServerError, "Failed to get video extension", err)
		return
	}
	videoMediaExt := videoMediaExts[0]

	urlRandBytes := make([]byte, 32)
	rand.Read(urlRandBytes)
	urlString := hex.EncodeToString(urlRandBytes)

	videoName := urlString + videoMediaExt

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
	}

	respondWithJSON(w, http.StatusOK, videoMetadata)
}
