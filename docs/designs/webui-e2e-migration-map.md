# 旧页 e2e 到 v2 的等价迁移清单

- 日期：2026-11-25
- 用途：**删旧页门禁③** 的证据。旧页 `web/e2e/*_e2e_test.go` 共 88 个 UI 用例（另有 18 个纯 API 用例），
  本表逐条给出 v2 的等价断言或明确边界。
- 结论口径：
  - **等价**：v2 有同语义断言（多数收敛进 `web/e2e/v2_shell_e2e_test.go::TestV2Shell_RouteThemeAndEntry`）；
  - **边界**：v2 有意不做（服务端有实现但页面不重复）或未覆盖，需要在下线前确认取舍。
- 迁移方式：**不是把旧用例复制成 v2 用例**，而是把「用户可见行为」在 v2 里断言一次；
  旧用例保留到旧页删除时一并移除。

## 一、逐条映射

| 旧用例文件 | 旧用例 | v2 等价断言 / 边界 |
| --- | --- | --- |
| e2e\audit_export_notify_test_e2e_test.go | `TestAudit_ExportButton` | V2 审计导出 NDJSON |
| e2e\audit_export_notify_test_e2e_test.go | `TestAudit_FilterRows` | V2 审计筛选 |
| e2e\audit_export_notify_test_e2e_test.go | `TestStatsPanel_NotifyTestButton` | V2 通知渠道自检 |
| e2e\audit_webui_e2e_test.go | `TestAuditWebUI_CreateVault_SwitchList_PlayRange` | V2 Secret 卷 + 预览 |
| e2e\auth_config_e2e_test.go | `TestAuth_RegisterTOTP_AndLogin` | V2 TOTP 注册 + 登录 |
| e2e\auth_config_e2e_test.go | `TestConfig_UpdateMaxStorage` | V2 配置分区保存 |
| e2e\auth_config_e2e_test.go | `TestVolumes_SingleVolumeSelect` | V2 卷下拉 |
| e2e\cloud_audit_e2e_test.go | `TestCloudDownload_SubmitCompleteRemove` | V2 云任务提交/完成/删除 |
| e2e\cloud_audit_e2e_test.go | `TestCloudDownload_Cancel` | V2 云任务取消 |
| e2e\cloud_audit_e2e_test.go | `TestCloudDownload_DamagedBadge_RealLink` | V2 任务状态 pill（损坏徽标未单列） |
| e2e\cloud_audit_e2e_test.go | `TestAudit_RendersSeededEvent` | V2 审计列表 |
| e2e\cloud_audit_e2e_test.go | `TestCloudDownload_MetaExtra` | V2 云任务转存 meta extra（页面未覆盖） |
| e2e\cloud_tasks_panel_e2e_test.go | `TestCloudTasksPanel_ShowsFailureAndRetries` | V2 任务失败 + 重试 |
| e2e\cloud_tasks_panel_e2e_test.go | `TestCloudTasksPanel_DetailAndBatchRetry` | V2 详情 + 批量重试 |
| e2e\console_layout_e2e_test.go | `TestConsoleLayout_FullscreenNavAndDeepLink` | V2 控制台深链 |
| e2e\console_layout_e2e_test.go | `TestConsoleOverviewAndFilters` | V2 概览 + 筛选 |
| e2e\console_layout_e2e_test.go | `TestConsoleCardsJumpAndPaging` | V2 卡片跳转 + 分页 |
| e2e\console_layout_e2e_test.go | `TestConsoleExportAndTrend` | V2 导出 + 趋势 |
| e2e\console_layout_e2e_test.go | `TestConsoleImportTSVRetries` | V2 TSV 导入 |
| e2e\console_layout_e2e_test.go | `TestConsoleDeepLinkFilters` | V2 深链筛选恢复 |
| e2e\console_layout_e2e_test.go | `TestConsoleCardJumpWritesDeepLink` | V2 卡片写深链 |
| e2e\console_layout_e2e_test.go | `TestConsoleKeyboardNav` | V2 Alt 分区跳转 |
| e2e\console_layout_e2e_test.go | `TestConsoleIssuesExportAndSizeFilter` | V2 事项导出 + 体积筛选 |
| e2e\console_layout_e2e_test.go | `TestConsoleHelpPanel` | V2 ? 帮助面板 |
| e2e\console_layout_e2e_test.go | `TestConsoleVolumeChipsAndActionLog` | V2 卷 chips + 最近操作 |
| e2e\credentials_sync_mesh_e2e_test.go | `TestStatsPanel_CredentialsTab` | V2 接入分区（凭据摘要） |
| e2e\credentials_sync_mesh_e2e_test.go | `TestStatsPanel_SyncConflictsTab` | V2 同步冲突（服务端无视图，RETIRED） |
| e2e\credentials_sync_mesh_e2e_test.go | `TestStatsPanel_MeshStatusTab` | V2 Mesh 分区 |
| e2e\events_e2e_test.go | `TestEvents_AutoRefreshAfterUpload` | V2 上传后列表刷新 |
| e2e\events_e2e_test.go | `TestEvents_AutoRefreshAfterDelete` | V2 删除后列表刷新 |
| e2e\files_e2e_test.go | `TestFiles_Mkdir` | V2 新建目录 |
| e2e\files_e2e_test.go | `TestFiles_Breadcrumb` | V2 面包屑 |
| e2e\files_e2e_test.go | `TestFiles_Search` | V2 搜索 |
| e2e\files_e2e_test.go | `TestFiles_Delete` | V2 删除（软删） |
| e2e\files_e2e_test.go | `TestFiles_BatchDelete` | V2 批量删除 |
| e2e\files_e2e_test.go | `TestFiles_Rename` | V2 重命名 |
| e2e\files_e2e_test.go | `TestFiles_BatchRename` | V2 批量重命名 |
| e2e\files_e2e_test.go | `TestFiles_Rmdir` | V2 删除目录（force） |
| e2e\force_refresh_e2e_test.go | `TestForceRefresh_ButtonRefreshesCacheThenLists` | V2「清缓存」+ 列表 |
| e2e\force_refresh_e2e_test.go | `TestForceRefresh_IncludesVolumeWhenSelected` | V2 清缓存带 volume 参数 |
| e2e\i18n_e2e_test.go | `TestWebUI_I18N_LangToggle` | V2 语言切换 |
| e2e\issues_e2e_test.go | `TestStatsPanel_IssuesTab` | V2 待处理事项分区 |
| e2e\issues_e2e_test.go | `TestIssuesTab_ScanRequest` | V2 事项（扫描由服务端周期任务执行，页面无按钮） |
| e2e\mesh_status_e2e_test.go | `TestMeshStatus_RendersCard` | V2 Mesh 分区 |
| e2e\meta_extra_e2e_test.go | `TestMetaExtra_Panel` | V2 检查器 Meta extra 编辑（本轮新增） |
| e2e\meta_extra_e2e_test.go | `TestMetaExtra_Empty` | V2 检查器 Meta 空态 |
| e2e\notify_panel_test.go | `TestStatsPanel_NotifySection` | V2 通知分区 |
| e2e\owner_login_e2e_test.go | `TestAuth_RegisterThenOwnerLogin` | V2 TOTP 注册 + owner 登录 |
| e2e\player_fallback_e2e_test.go | `TestPlayerFallback_UnsupportedFormat` | V2 预览不支持格式提示 |
| e2e\quota_e2e_test.go | `TestE2E_QuotaPanel_RendersFederatedView` | V2 卷分区（配额视图未单列，见边界） |
| e2e\share_version_e2e_test.go | `TestShare_CreateAndPublicAccess` | V2 分享创建（v2 未覆盖 /s/{token} 公开访问） |
| e2e\share_version_e2e_test.go | `TestVersioning_UploadCreatesVersions` | V2 版本页签 |
| e2e\share_version_e2e_test.go | `TestVersioning_DisabledReturns501` | V2 版本页签 501 提示 |
| e2e\sync_carrier_badge_e2e_test.go | `TestSyncCarrierBadge_RendersFromServerData` | V2 同步任务（载体列：kind · transport · carriers） |
| e2e\sync_carrier_badge_e2e_test.go | `TestSyncCarrierBadge_MeshRemoteShowsDeclaredTransport` | 同上 |
| e2e\trash_view_e2e_test.go | `TestTrashView_Panel` | V2 回收站 |
| e2e\trash_view_e2e_test.go | `TestTrashView_MultiVolume_VolumeColumnFilterAndEmpty` | V2 回收站多卷列 |
| e2e\ui_e2e_test.go | `TestFileList` | V2 文件列表（v2_shell_e2e_test.go 列表断言） |
| e2e\ui_e2e_test.go | `TestDirectoryNavigation` | V2 子目录深链 + 面包屑 |
| e2e\ui_e2e_test.go | `TestUploadButton` | V2 上传（简单 + 分块） |
| e2e\ui_e2e_test.go | `TestDownloadLink` | V2 单文件下载（签名 URL） |
| e2e\ui_e2e_test.go | `TestBatchToolbar` | V2 批量删除/重命名/打包 |
| e2e\ui_e2e_test.go | `TestStatsPanel` | V2 控制台概览 |
| e2e\ui_e2e_test.go | `TestMkdirButton` | V2 新建目录 |
| e2e\ui_e2e_test.go | `TestCloudDownloadButtonExists` | V2 传输页云端任务 |
| e2e\ui_e2e_test.go | `TestCloudDownloadModalOpens` | V2 传输页「新建下载」对话框 |
| e2e\ui_e2e_test.go | `TestCloudDownloadModalCloses` | 同上（关闭） |
| e2e\ui_e2e_test.go | `TestCloudDownloadIntegrityBadge` | V2 任务行状态 pill（完整性徽标未单列） |
| e2e\ui_e2e_test.go | `TestCloudDownloadUnverifiedBadge` | 同上 |
| e2e\ui_e2e_test.go | `TestCloudDownloadTaskList` | V2 任务列表 |
| e2e\ui_e2e_test.go | `TestCloudDownloadURLInput` | V2 新建下载 URL 输入 |
| e2e\ui_e2e_test.go | `TestShareAPI` | V2 检查器分享页签 |
| e2e\ui_e2e_test.go | `TestVersioningButton` | V2 检查器版本页签 |
| e2e\ui_e2e_test.go | `TestVersioningModalOpenClose` | 同上 |
| e2e\ui_e2e_test.go | `TestDirArchiveButton` | V2 打包下载（所选） |
| e2e\ui_e2e_test.go | `TestSearchFunction` | V2 文件搜索 |
| e2e\ui_e2e_test.go | `TestStorageConfigInStats` | V2 配置分区（只读事实） |
| e2e\ui_e2e_test.go | `TestStorageConfigAPI` | API 层（无 UI 依赖） |
| e2e\ui_e2e_test.go | `TestHubTab` | V2 Hub 分区 |
| e2e\volume_ops_preview_e2e_test.go | `TestVolumes_OpsButtons` | V2 卷分区操作（用户卷建删） |
| e2e\volume_ops_preview_e2e_test.go | `TestPreview_ImageUsesThumbnail` | V2 图片预览 |
| e2e\volume_path_reload_e2e_test.go | `TestVolumePathReload_ListKeepsVolume` | V2 卷过滤持久化 |
| e2e\volume_select_refresh_e2e_test.go | `TestVolumes_LoginRefreshesSelect` | V2 登录后刷新卷下拉 |
| e2e\volume_select_refresh_e2e_test.go | `TestVolumes_SelectPersistsAcrossReload` | V2 卷选择持久化 |
| e2e\volume_select_refresh_e2e_test.go | `TestVolumes_PerVolumeSubdirMemory` | V2 按卷子目录记忆（未实现，边界） |
| e2e\volume_select_refresh_e2e_test.go | `TestVolumesPanel_RefreshButtonTriggersAPI` | V2 卷分区刷新 |
| e2e\volumes_e2e_test.go | `TestVolumes_Panel` | V2 卷分区 |
| e2e\volumes_e2e_test.go | `TestVolumes_UploadVolumeSelect` | V2 上传到指定卷（未实现，边界） |

## 二、v2 未覆盖项（下线前需确认）

| 项 | 现状 | 建议 |
| --- | --- | --- |
| `/s/{token}` 公开分享页 | v2 只覆盖「创建分享」；公开页与 UI 壳无关（服务端独立渲染 JSON/文件），保留为独立入口 | **不阻塞下线**（决定：保留） |
| 云端任务完整性徽标 | ✅ 已补：`integrity_status` → 完整/损坏 badge（`cloud-table.js`） | — |
| 同步任务载体徽标 | ✅ 已补：同步表新增「载体」列（kind · transport · 终态 carriers 计数） | — |
| 按卷记忆子目录 | ✅ 已补：`sproxy_v2_subdir_by_vol` 记录每卷上次目录，切卷自动跳回 | — |
| 上传到指定卷 | ✅ 已补：简单上传 `?volume=`、分块 init body `volume`（e2e 实测落卷） | — |
| 事项手动扫描 | ✅ 已补：「立即扫描」按钮调用 `POST /api/issues/scan` 并回显登记数 | — |

## 三、删旧页执行顺序（门禁全绿后）

1. 本表「未覆盖项」全部确认取舍（补实现或明确不做）；
2. 旧页 `/ui/` 静态文件与 `app.js` 系列删除，`/ui/` 301 → `/ui/v2/`；
3. 旧页 e2e（本表第一列）随之删除，并把 v2 用例补到同等覆盖；
4. 文档更新：`docs/api.md` 的 UI 入口、`README`、设计文档「旧页下线」小节；
5. 单独 PR + PR body 附本表快照。
