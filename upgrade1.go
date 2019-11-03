package cony

import (
	"code.byted.org/toutiao/mpaas/consts"
	"code.byted.org/toutiao/mpaas/service/git_lab_dev"
	"code.byted.org/toutiao/mpaas/service/larkrobot"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	optimus_biz "code.byted.org/toutiao/mpaas/business/optimus"
	repo_upgrade_biz "code.byted.org/toutiao/mpaas/business/repo/upgrade"
	"code.byted.org/toutiao/mpaas/lib"
	"code.byted.org/toutiao/mpaas/model"
	"code.byted.org/toutiao/mpaas/service/db"
	"code.byted.org/toutiao/mpaas/utils"
	"code.byted.org/toutiao/mpaas/utils/app_info"
)

const (
	OPTIMUS_UPGRADE_VERSION_TYPE_AUTO  = "null"
	OPTIMUS_UPGRADE_VERSION_TYPE_FIXED = "fixed"
	OPTIMUS_UPGRADE_VERSION_TYPE_SEM   = "fixed"
)

type conySourceUpgradeRepoResponseMain struct {
	*SourceUpgradeRepoReq
	VersionFinal string `json:"version_final"`
	CommitId     string `json:"commit_id"`
}

//SourceUpgradeRepoResponseRet 源码升级上报给 Optimus 的数据格式
type SourceUpgradeRepoResponseRet struct {
	conySourceUpgradeRepoResponseMain
	Status  string `json:"status"`
	Message string `json:"message"`
}

//SourceUpgradeRepoResponseData源码升级直接返回的response的data
type SourceUpgradeRepoResponseData struct {
	HistoryID int `json:"history_id"`
}

//SourceUpgradeRepoReq Cony 发版接口参数定义
type SourceUpgradeRepoReq struct {
	GroupName         string                   `json:"group_name" valid:"NotEmpty"`  // 业务线英文名称
	ProjectId         int                      `json:"project_id" valid:"int-min=1"` // 业务线仓库 Gitlab Id
	ServiceId         int                      `json:"service_id"`                   // Optimus 字段，目前无用
	ProjectName       string                   `json:"project_name"`                 // 组件仓库名称
	MrIid             int                      `json:"mr_iid"`                       // mr id
	PodspecName       string                   `json:"podspec_name"`                 // 组件 Podspec 名称
	ProjectUrl        string                   `json:"project_url"`                  // 组件仓库地址
	SourceUrl         string                   `json:"source_url"`                   // 组件 source 推仓地址
	ShouldIntegration *bool                    `json:"should_integration"`           // 此次发版是否需要集成
	Version           conySourceUpgradeVersion `json:"version" valid:"NotEmpty"`     // 升级的版本号
	Branch            string                   `json:"branch" valid:"NotEmpty"`      // 用哪个分支升级
	Username          string                   `json:"username" valid:"NotEmpty"`    // 此次升级的用户名称
	NeedPublish       bool                     `json:"need_publish"`                 // 是否需要源码发版
	CommitId          string                   `json:"commit_id"`                    // 发版的commit id
	NeedBinary        bool                     `json:"need_binary"`                  // 是否需要二进制发版, 支持源码发版的时候同时二进制发版
}

type conySourceUpgradeVersion struct {
	Type        string `json:"type"`         // 类型，取值只能为 auto 或者 fixed
	VersionBase string `json:"version_base"` // 如果类型为 auto，这里表示基础版本号，需要计算，如果类型为 fixed 以这个为准
}

//SourceUpgradeVersionDetail 组件升级版本号数据结构
type SourceUpgradeVersionDetail struct {
	VersionBase  string `json:"version_base"`  // 如果是 auto，增加最后一位，如果是 fixed，补上后缀，如果是语义化，根据语义化规则计算
	Suffix       string `json:"suffix"`        // 后缀，如果是语义化，表示 patch
	UpgradeType  string `json:"upgrade_type"`  // 如果是语义化，表示升级哪一位，取值：major/minor/patch
	CustomSuffix string `json:"custom_suffix"` // 自定义后缀
}

//GenerateComponentUpgradeReqFromCony 根据 Cony 传递的参数，拼接出需要升级组件的参数
func GenerateComponentUpgradeReqFromCony(ctx context.Context, req *SourceUpgradeRepoReq) (*repo_upgrade_biz.MpaasRepoUpgradeReq, error) {
	repoProject, err := db.GetProjectDetailByGitlabId(ctx, req.ProjectId)
	if err != nil {
		return nil, err
	}

	versionDetail := &SourceUpgradeVersionDetail{}
	err = json.Unmarshal([]byte(req.Version.VersionBase), &versionDetail)
	if err != nil {
		return nil, err
	}

	if len(versionDetail.VersionBase) == 0 && !isAutoAndNoIntegratePublish(req) { //版本号为空, 并且非独立发版
		return nil, errors.New("空版本号禁止发版")
	}

	if err = lib.ValidateSuffix(versionDetail.CustomSuffix); err != nil {
		return nil, err
	}

	//根据所在业务线和组件的 project 信息，找到精确的组件
	repo, err := searchRepoByReq(ctx, req, repoProject)
	if err != nil || repo == nil {
		return nil, fmt.Errorf("业务线 %v 下找不到组件 %v(%v)", req.GroupName, req.ProjectName, req.ProjectId)
	}

	// 找到组件真实所属的业务线，用于兜底计算 source
	repoAppID, _ := strconv.Atoi(repo.AppID)
	repoBelongApp, _ := db.GetAppByAppId(int64(repoAppID))

	source := req.SourceUrl
	if len(source) == 0 {
		var projectOptimusConfig map[string]interface{}
		err = json.Unmarshal([]byte(repoProject.Config), &projectOptimusConfig)
		if err != nil {
			source = app_info.GetAppSource(repoBelongApp)
		}
		if projectOptimusConfigSource, ok := projectOptimusConfig["source_url"].(string); ok {
			source = projectOptimusConfigSource
		} else {
			source = app_info.GetAppSource(repoBelongApp)
		}
	}

	repoCid := req.CommitId // cony传了commit直接用cony的, 否则获取分支最后的commit
	if len(repoCid) == 0 {
		repoCid, err = git_lab_dev.GetLastestCommitId(ctx, repoProject.GitId, req.Branch)
	}

	finalVersion, err := getFinalVersion(ctx, req, versionDetail, repo)
	if err != nil {
		return nil, err
	}

	localNeedPublish := false
	optimusConfig := optimus_biz.ParseOptimusProjectConfig(repoProject)
	if optimusConfig != nil {
		localNeedPublish = optimusConfig.AutoPublish && optimusConfig.AutoPublishType == model.OptimusConfigAutoPublishTypeBytebus
	}

	responseForOptimus := getOptimusRetMain(finalVersion, repoCid, req)
	upgradeReq := &repo_upgrade_biz.MpaasRepoUpgradeReq{
		RepoID:      repo.Id,
		Branch:      req.Branch,
		CommitID:    repoCid,
		MrIid:       req.MrIid,
		Version:     finalVersion,
		Username:    req.Username,
		SaveHistory: true,
		IosExtParams: repo_upgrade_biz.MpaasRepoUpgradeIosExtension{
			SourceAddress:   source,
			OptimusResponse: utils.ToJson(responseForOptimus),
			OnlyBinary:      !req.NeedPublish && req.NeedBinary,
		},
		FromType:    repo_upgrade_biz.UPGRADE_FROM_TYPE_CONY,
		SkipPublish: !req.NeedPublish && !req.NeedBinary, // 不需要发版, 也不需要二进制
	}
	if shouldIntegrate(req) {
		// 如果需要发版，这里就强制指定只发源码版本
		upgradeReq.IosExtParams.OnlySource = true
	} else {
		// 如果是不集成，则自己处理发版问题，需要找到创建 MR 时记录的 changeLog
		upgradeReq.ChangeLog = db.GetRepoChangeLogFromConyMrInfo(ctx, req.ProjectId, req.MrIid)
	}

	// 校验是否需要告警
	go validNeedPublishNeedReport(req, req.NeedPublish, localNeedPublish, repo)

	return upgradeReq, nil
}

// 判断是否需要集成
func shouldIntegrate(req *SourceUpgradeRepoReq) bool {
	// 如果不传参数，说明是老逻辑，表示也需要集成
	return req.ShouldIntegration == nil || *req.ShouldIntegration
}

// 是否是auto类型并且不需要集成
func isAutoAndNoIntegratePublish(req *SourceUpgradeRepoReq) bool {
	return req.Version.Type == OPTIMUS_UPGRADE_VERSION_TYPE_AUTO && !shouldIntegrate(req)
}

func getFinalVersion(ctx context.Context, req *SourceUpgradeRepoReq, versionDetail *SourceUpgradeVersionDetail, repo *model.ArchPodMain) (string, error) {
	finalVersion := ""

	if len(versionDetail.VersionBase) == 0 && isAutoAndNoIntegratePublish(req) { //空版本号, 并且是独立发版
		history, err := db.GetPodLastHistory(ctx, repo.Id)
		if err != nil {
			return "", err
		}
		finalVersion = lib.GetRepoNextVersion(ctx, repo.Id, history.Version, consts.VALID_VERSION_UPGRADE_TYPE_PATCH,
			consts.VALID_VERSION_RELEASE_TYPE_RELEASE, versionDetail.Suffix)
		return finalVersion, nil
	}

	switch req.Version.Type {
	case OPTIMUS_UPGRADE_VERSION_TYPE_AUTO:
		finalVersion = lib.GetRepoNextAutoVersion(ctx, repo.Id, versionDetail.VersionBase, versionDetail.Suffix)
	case OPTIMUS_UPGRADE_VERSION_TYPE_FIXED:
		if len(versionDetail.Suffix) > 0 {
			finalVersion = fmt.Sprintf("%s-%s", versionDetail.VersionBase, versionDetail.Suffix)
		} else {
			finalVersion = versionDetail.VersionBase
		}
	case OPTIMUS_UPGRADE_VERSION_TYPE_SEM:
		finalVersion = lib.GetRepoNextVersion(ctx, repo.Id, versionDetail.VersionBase, versionDetail.UpgradeType, versionDetail.Suffix, versionDetail.CustomSuffix)
	default:
		return "", errors.New("版本号类型不合法")
	}

	return finalVersion, nil
}

func searchRepoByReq(ctx context.Context, req *SourceUpgradeRepoReq, repoProject *model.MpaasProjectDetail) (*model.ArchPodMain, error) {
	if shouldIntegrate(req) {
		// 如果需要集成，则根据主仓信息来搜索组件

		// 这个 repoUsedInApp 表示组件在哪个业务线使用，比如 FrameworkDemos，虽然归属于内网公共库，但这里可能传过来头条，表示这个组件在头条里面合码
		repoUsedInApp, err := db.GetAppByEnName(ctx, req.GroupName)
		if err != nil {
			return nil, err
		}
		return db.GetRelatedRepoWithProjectIdInApp(ctx, repoProject.Id, repoUsedInApp.Id)
	} else {
		// 如果不要集成，表示单仓独立发版，通过 里面预留的数据，
		repoID, err := db.GetRepoIDFromConyMrInfo(ctx, req.ProjectId, req.MrIid)

		// 如果找不到组件信息，说明可能直接通过 Cony 操作。这时候可以尝试兼容下，如果能找到唯一的组件，就继续流程，否则报错
		if err != nil {
			repos, _ := db.GetReposByGitlabId(ctx, req.ProjectId)

			// 说明找不到组件
			if repos == nil || len(repos) == 0 {
				return nil, fmt.Errorf("找不到 projectId = %d 的组件", req.ProjectId)
			}

			// 找到多各个组件，无法确认用哪个，因此拒绝发版，并且提示用户通过组件平台来操作
			if len(repos) > 1 {
				return nil, fmt.Errorf("找到多个 projectId = %d 的组件，平台无法发版，请参考：http://mobile.bytedance.net/docs/manual/11810/", req.ProjectId)
			}
			return repos[0], nil
		}
		return db.GetPodMainById(ctx, int64(repoID))
	}
}

//getOptimusRetMain 返回 Optimus 结构的主体内容
func getOptimusRetMain(finalVersion string, commitID string, req *SourceUpgradeRepoReq) conySourceUpgradeRepoResponseMain {
	return conySourceUpgradeRepoResponseMain{
		SourceUpgradeRepoReq: req,
		VersionFinal:         finalVersion,
		CommitId:             commitID,
	}
}

//GenOptimusRet 构造 Optimus 接口的情况返回值
func GenOptimusRet(success bool, req *SourceUpgradeRepoReq, version string, commitID string, message string) SourceUpgradeRepoResponseRet {
	mainResponse := getOptimusRetMain(version, commitID, req)
	status := "failed"
	if success {
		status = "succeed"
	}
	return SourceUpgradeRepoResponseRet{
		conySourceUpgradeRepoResponseMain: mainResponse,
		Status:                            status,
		Message:                           message,
	}
}

func GetOptimusResponseData(historyId int) SourceUpgradeRepoResponseData {
	return SourceUpgradeRepoResponseData{
		HistoryID: historyId,
	}
}

// 校验optimus是否需要真的发版, 如果传入的和数据库不一致就告警
func validNeedPublishNeedReport(req *SourceUpgradeRepoReq, needPublish bool, localNeedPublish bool, repo *model.ArchPodMain) {
	if needPublish == localNeedPublish { //一致直接返回
		return
	}

	// 发送告警信息
	msg := fmt.Sprintf("Optimus与Bytebus发版不一致\noptimus: need_publish=%t, \nproject: need_publish=%t\n\n%s",
		needPublish, localNeedPublish, lib.RepoDetailURLByPodWithType(repo, lib.ComponentTabTypeDefault))
	larkrobot.AlarmRobot([]string{msg})
}
