package controllers

import (
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/cloudreve/Cloudreve/v4/service/team"
	"github.com/gin-gonic/gin"
)

// ListTeams lists teams for the current user.
func ListTeams(c *gin.Context) {
	res, err := team.List(c)
	if err != nil {
		c.JSON(200, serializer.Err(c, err))
		return
	}
	c.JSON(200, serializer.Response{Data: res})
}

// TeamDirectory lists all teams for transfer destination pickers.
func TeamDirectory(c *gin.Context) {
	res, err := team.Directory(c)
	if err != nil {
		c.JSON(200, serializer.Err(c, err))
		return
	}
	c.JSON(200, serializer.Response{Data: res})
}
