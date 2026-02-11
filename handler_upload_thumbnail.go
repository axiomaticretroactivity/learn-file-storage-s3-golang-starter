package main

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
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

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to parse file", err)
		return
	}

	thumbnailImage, thumbnailHeaderPtr, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to form file", err)
		return
	}
	defer thumbnailImage.Close()

	videoMetadata, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}

	if videoMetadata.UserID != userID {
		respondWithError(w, http.StatusForbidden, "Video does not belong to current user", err)
		return
	}

	thumbnailMediaType := thumbnailHeaderPtr.Header.Get("Content-Type")
	parsedType, _, err := mime.ParseMediaType(thumbnailMediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Missing or malformed Content-Type header", err)
		return
	}
	if parsedType != "image/jpg" && parsedType != "image/png" {
		respondWithError(w, http.StatusUnsupportedMediaType, "Video thumbnails must be in jpg or png format", fmt.Errorf("bad upload received (thumbnail not png or jpg)"))
		return
	}

	thumbnailMediaExts, err := mime.ExtensionsByType(thumbnailMediaType)
	if err != nil || len(thumbnailMediaExts) == 0 {
		respondWithError(w, http.StatusInternalServerError, "Failed to get thumbnail image extension", err)
		return
	}
	thumbnailMediaExt := thumbnailMediaExts[0]

	thumbnailFilePath := filepath.Join(cfg.assetsRoot, videoIDString+thumbnailMediaExt)

	thumbnailNewFile, err := os.Create(thumbnailFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create file", err)
		return
	}
	defer thumbnailNewFile.Close()

	_, err = io.Copy(thumbnailNewFile, thumbnailImage)
	if err != nil {
		respondWithError(w, http.StatusInsufficientStorage, "Could not write new file", err)
		return
	}

	thumbnailURL := fmt.Sprintf("http://localhost:%s/assets/%s%s", cfg.port, videoIDString, thumbnailMediaExt)

	videoMetadata.ThumbnailURL = &thumbnailURL
	videoMetadata.UpdatedAt = time.Now()

	err = cfg.db.UpdateVideo(videoMetadata)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video", err)
	}

	respondWithJSON(w, http.StatusOK, videoMetadata)
}
