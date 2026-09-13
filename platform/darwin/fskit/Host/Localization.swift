import Foundation

enum L10nKey: String {
    case productName
    case statusHealthy
    case statusRecovering
    case statusAttention
    case statusUnavailable
    case statusUnknown
    case statusSummaryHealthy
    case statusSummaryRecovering
    case statusSummaryAttention
    case statusSummaryUnavailable
    case openDashboard
    case checkStatus
    case exportDiagnostics
    case copyDiagnostics
    case revealStatusFiles
    case quit
    case overview
    case components
    case updated
    case neverUpdated
    case noComponents
    case unconnectedTitle
    case unconnectedDetail
    case loginItemNotConfigured
    case loginItemRequiresApproval
    case loginItemUnavailable
    case incidentMonitorNotConfigured
    case incidentMonitorRequiresApproval
    case incidentMonitorUnavailable
    case administratorRecoveryRecommendation
    case administratorCapabilityMemberDetail
    case administratorCapabilityNotMemberDetail
    case administratorCapabilityUnknownDetail
    case incidentTitle
    case incidentRecoveredTitle
    case incidentRecoveredBody
    case reason
    case impact
    case recommendedActions
    case technicalDetails
    case close
    case save
    case diagnosticsSaved
    case diagnosticsSaveFailed
    case stale
    case waitingForStatus
    case managedSessions
    case logicalBytes
    case physicalBytes
    case spaceSaved
    case savingsRate
    case sessionDataSize
    case diskUsage
    case storageHistory
    case ioActivity
    case readRate
    case writeRate
    case noActivityHistory
    case recentIncidents
    case noStorageHistory
    case noRecentIncidents
    case rangeOneHour
    case rangeOneDay
    case rangeSevenDays
    case rangeThirtyDays
    case originalSize
    case currentUsage
    case incidentOngoing
    case incidentRecovered
    case lastUpdatedFormat
    case unknownReason
    case unknownImpact
    case retryRecommendation
    case openDashboardRecommendation
    case daemonStatusChannelSummary
    case daemonStatusChannelRecoveredSummary
    case daemonStatusChannelReason
    case daemonStatusChannelImpact
    case daemonStatusChannelWaitRecommendation
    case daemonStatusChannelMissingDetail
    case daemonStatusChannelInvalidDetail
    case daemonStatusChannelContinuityInvalidDetail
    case daemonStatusChannelStaleDetail
    case managedStatusChannelSummary
    case managedStatusChannelRecoveredSummary
    case managedStatusChannelReason
    case managedStatusChannelImpact
    case managedStatusChannelWaitRecommendation
    case managedStatusChannelMissingDetail
    case managedStatusChannelInvalidDetail
    case managedStatusChannelContinuityInvalidDetail
    case managedStatusChannelStaleDetail
    case supervisorStatusChannelSummary
    case supervisorStatusChannelRecoveredSummary
    case supervisorStatusChannelReason
    case supervisorStatusChannelImpact
    case supervisorStatusChannelWaitRecommendation
    case supervisorStatusChannelMissingDetail
    case supervisorStatusChannelInvalidDetail
    case supervisorStatusChannelContinuityInvalidDetail
    case supervisorStatusChannelStaleDetail
    case supervisorRecoveryReason
    case supervisorFailureImpact
    case frontend
    case managed
    case supervisor
    case daemon
    case storage
    case autoFold
    case autoFoldDetail
    case autoFoldCheckInterval
    case autoFoldIdleFor
    case autoFoldScope
    case autoFoldBatch
    case autoFoldBatchAutomatic
    case autoFoldBatchCountFormat
    case autoFoldScopeArchived
    case autoFoldScopeArchivedAndIdle
    case autoFoldProgressFoldedFormat
    case autoFoldProgressFoldedOnlyFormat
    case autoFoldChecking
    case autoFoldFolding
    case autoFoldReclaiming
    case autoFoldReclaimWaiting
    case autoFoldOff
    case autoFoldWaitingForCheck
    case autoFoldRetry
    case autoFoldConfigurationError
    case autoFoldCheckSoon
    case autoFoldCheckUnderTwoMinutes
    case autoFoldCheckMinutesFormat
    case autoFoldCheckHoursFormat
    case autoFoldSaveFailed
    case durationThirtyMinutes
    case durationTwoHours
    case durationSixHours
    case durationOneDay
}

enum L10n {
    private enum Language {
        case simplifiedChinese
        case traditionalChinese
        case english
    }

    private static let language: Language = {
        let identifiers = Locale.preferredLanguages.map { $0.lowercased() }
        guard let identifier = identifiers.first else { return .english }
        if identifier.hasPrefix("zh-hant") || identifier.hasPrefix("zh-tw") || identifier.hasPrefix("zh-hk") {
            return .traditionalChinese
        }
        if identifier.hasPrefix("zh") {
            return .simplifiedChinese
        }
        return .english
    }()

    static func text(_ key: L10nKey) -> String {
        switch language {
        case .simplifiedChinese:
            return simplifiedChinese[key] ?? english[key] ?? key.rawValue
        case .traditionalChinese:
            return traditionalChinese[key] ?? english[key] ?? key.rawValue
        case .english:
            return english[key] ?? key.rawValue
        }
    }

    static func format(_ key: L10nKey, _ arguments: CVarArg...) -> String {
        String(format: text(key), locale: Locale.current, arguments: arguments)
    }

    private static let simplifiedChinese: [L10nKey: String] = [
        .productName: "CodexFold",
        .statusHealthy: "运行正常",
        .statusRecovering: "正在恢复",
        .statusAttention: "需要处理",
        .statusUnavailable: "状态不可用",
        .statusUnknown: "等待状态",
        .statusSummaryHealthy: "CodexFold 运行正常",
        .statusSummaryRecovering: "文件访问正在自动恢复",
        .statusSummaryAttention: "文件访问需要处理",
        .statusSummaryUnavailable: "尚未连接到 CodexFold 文件服务",
        .openDashboard: "打开 CodexFold",
        .checkStatus: "立即检查",
        .exportDiagnostics: "导出诊断",
        .copyDiagnostics: "复制诊断",
        .revealStatusFiles: "在 Finder 中显示状态文件",
        .quit: "退出 CodexFold 菜单栏",
        .overview: "概览",
        .components: "运行状态",
        .updated: "更新时间",
        .neverUpdated: "尚未收到状态",
        .noComponents: "正在等待 CodexFold 报告状态",
        .unconnectedTitle: "文件服务尚未连接",
        .unconnectedDetail: "完成 CodexFold 安装并启动服务后，这里会显示节省空间和运行状态。",
        .loginItemNotConfigured: "尚未开启登录后自动启动。完成安装后可随系统登录运行。",
        .loginItemRequiresApproval: "请在“系统设置 > 通用 > 登录项”中允许 CodexFold 登录后启动。",
        .loginItemUnavailable: "CodexFold 暂时无法设置登录后启动，文件服务不受影响。",
        .incidentMonitorNotConfigured: "尚未开启后台故障提醒。完成安装后可在 CodexFold 未打开时继续提醒。",
        .incidentMonitorRequiresApproval: "请在“系统设置 > 通用 > 登录项”中允许 CodexFold 故障提醒；后台文件服务不会因此重启。",
        .incidentMonitorUnavailable: "CodexFold 暂时无法常驻显示故障提醒；菜单栏仍会在运行时显示故障窗口。",
        .administratorRecoveryRecommendation: "如需管理员级修复，请只从你明确点击的 CodexFold 操作进入系统授权；CodexFold 不会自动请求密码或使用 sudo。",
        .administratorCapabilityMemberDetail: "本地授权能力：当前账户属于 admin 组；未探测 sudo 缓存；自动提权已禁用；任何系统授权都必须由用户明确发起。",
        .administratorCapabilityNotMemberDetail: "本地授权能力：当前账户不属于 admin 组；未探测 sudo 缓存；自动提权已禁用；任何系统授权都必须由用户明确发起。",
        .administratorCapabilityUnknownDetail: "本地授权能力：无法只读确认当前账户是否属于 admin 组；未探测 sudo 缓存；自动提权已禁用；任何系统授权都必须由用户明确发起。",
        .incidentTitle: "CodexFold 需要处理",
        .incidentRecoveredTitle: "文件访问已恢复",
        .incidentRecoveredBody: "Codex 可以继续正常使用",
        .reason: "发生了什么",
        .impact: "影响",
        .recommendedActions: "你现在可以这样做",
        .technicalDetails: "技术详情",
        .close: "关闭",
        .save: "保存",
        .diagnosticsSaved: "诊断已导出",
        .diagnosticsSaveFailed: "诊断导出失败",
        .stale: "状态已过期",
        .waitingForStatus: "等待后台状态文件",
        .managedSessions: "已管理会话",
        .logicalBytes: "逻辑数据",
        .physicalBytes: "实际占用",
        .spaceSaved: "已节省",
        .savingsRate: "节省比例",
        .sessionDataSize: "会话原始大小",
        .diskUsage: "当前磁盘占用",
        .storageHistory: "空间变化",
        .ioActivity: "读写速度",
        .readRate: "读取",
        .writeRate: "写入",
        .noActivityHistory: "正在收集读写速度，几分钟后会显示趋势。",
        .recentIncidents: "最近故障",
        .noStorageHistory: "正在收集空间变化，几分钟后会显示趋势。",
        .noRecentIncidents: "最近没有记录到持续 10 秒的故障。",
        .rangeOneHour: "1 小时",
        .rangeOneDay: "24 小时",
        .rangeSevenDays: "7 天",
        .rangeThirtyDays: "30 天",
        .originalSize: "原始大小",
        .currentUsage: "磁盘占用",
        .incidentOngoing: "仍在发生",
        .incidentRecovered: "已恢复",
        .lastUpdatedFormat: "%@ 更新",
        .unknownReason: "CodexFold 前端持续无法完成健康检查。",
        .unknownImpact: "Codex 可能持续重连，打开或写入会话可能失败。",
        .retryRecommendation: "等待自动恢复，避免强制退出正在写入的 Codex。",
        .openDashboardRecommendation: "打开 CodexFold 查看组件状态并导出诊断。",
        .daemonStatusChannelSummary: "正在恢复后台服务监测",
        .daemonStatusChannelRecoveredSummary: "后台服务监测已恢复",
        .daemonStatusChannelReason: "CodexFold 已持续无法取得可信且持续更新的后台服务状态。",
        .daemonStatusChannelImpact: "Codex 会继续运行，CodexFold 不会操作或重启 Codex；在后台恢复前，会话打开或写入可能等待或失败。",
        .daemonStatusChannelWaitRecommendation: "保持 Codex 和 CodexFold 运行，等待后台服务自动恢复。",
        .daemonStatusChannelMissingDetail: "尚未收到可安全读取的后台服务状态报告。",
        .daemonStatusChannelInvalidDetail: "收到的后台服务状态报告不完整或无法安全读取。",
        .daemonStatusChannelContinuityInvalidDetail: "后台服务状态与耐久恢复基线不一致，暂时不能证明已经恢复。",
        .daemonStatusChannelStaleDetail: "后台服务状态报告已停止更新。",
        .managedStatusChannelSummary: "正在恢复会话状态监测",
        .managedStatusChannelRecoveredSummary: "会话状态监测已恢复",
        .managedStatusChannelReason: "CodexFold 已持续无法取得可信的会话管理状态。",
        .managedStatusChannelImpact: "Codex 会继续运行，现有文件服务不会因此被重启，但 CodexFold 暂时无法确认会话管理是否正常。",
        .managedStatusChannelWaitRecommendation: "保持 Codex 和 CodexFold 运行，等待状态监测自动恢复。",
        .managedStatusChannelMissingDetail: "尚未收到会话管理状态报告。",
        .managedStatusChannelInvalidDetail: "收到的会话管理状态报告不完整或无法安全读取。",
        .managedStatusChannelContinuityInvalidDetail: "会话管理状态与耐久恢复基线不一致，暂时不能证明已经恢复。",
        .managedStatusChannelStaleDetail: "会话管理状态报告已停止更新。",
        .supervisorStatusChannelSummary: "正在恢复文件服务守护监测",
        .supervisorStatusChannelRecoveredSummary: "文件服务守护监测已恢复",
        .supervisorStatusChannelReason: "CodexFold 已持续无法取得可信的文件服务守护状态。",
        .supervisorStatusChannelImpact: "Codex 会继续运行，CodexFold 不会退出、重启或操作 Codex，但当前无法确认文件服务守护是否仍在工作。",
        .supervisorStatusChannelWaitRecommendation: "保持 Codex 和 CodexFold 运行，等待守护监测自动恢复。",
        .supervisorStatusChannelMissingDetail: "尚未收到文件服务守护状态报告。",
        .supervisorStatusChannelInvalidDetail: "收到的文件服务守护状态报告不完整或无法安全读取。",
        .supervisorStatusChannelContinuityInvalidDetail: "文件服务守护状态与耐久恢复基线不一致，暂时不能证明已经恢复。",
        .supervisorStatusChannelStaleDetail: "文件服务守护状态报告已停止更新。",
        .supervisorRecoveryReason: "CodexFold 文件服务守护持续无法确认前端与后台连接都已恢复。",
        .supervisorFailureImpact: "Codex 会继续运行，挂载点保持存在；在自动恢复完成前，会话打开或写入可能等待或失败。",
        .frontend: "文件访问",
        .managed: "会话管理",
        .supervisor: "文件服务守护",
        .daemon: "后台服务",
        .storage: "存储",
        .autoFold: "自动折叠",
        .autoFoldDetail: "关掉后不再自动折。正在折的这一条会在当前步骤结束后停下，不用退出 Codex。",
        .autoFoldCheckInterval: "多久检查一次",
        .autoFoldIdleFor: "闲置多久才折",
        .autoFoldScope: "折哪些",
        .autoFoldBatch: "每轮折多少",
        .autoFoldBatchAutomatic: "自动",
        .autoFoldBatchCountFormat: "%d 个会话",
        .autoFoldScopeArchived: "只已归档",
        .autoFoldScopeArchivedAndIdle: "已归档和闲置中的",
        .autoFoldProgressFoldedFormat: "已折叠 %d · 还剩 %d",
        .autoFoldProgressFoldedOnlyFormat: "已折叠 %d",
        .autoFoldChecking: "正在检查闲置对话",
        .autoFoldFolding: "正在折叠",
        .autoFoldReclaiming: "正在校验并释放空间",
        .autoFoldReclaimWaiting: "等待正在使用的任务释放文件后继续回收",
        .autoFoldOff: "已关闭",
        .autoFoldWaitingForCheck: "打开后会按设定检查",
        .autoFoldRetry: "这一轮没折成，会按设定再试",
        .autoFoldConfigurationError: "自动折叠设置有问题，已暂停，请修复后再试。",
        .autoFoldCheckSoon: "即将再检查",
        .autoFoldCheckUnderTwoMinutes: "不到 2 分钟后再检查",
        .autoFoldCheckMinutesFormat: "约 %d 分钟后再检查",
        .autoFoldCheckHoursFormat: "约 %d 小时后再检查",
        .autoFoldSaveFailed: "自动折叠设置没能保存，请再试一次。",
        .durationThirtyMinutes: "30 分钟",
        .durationTwoHours: "2 小时",
        .durationSixHours: "6 小时",
        .durationOneDay: "1 天",
    ]

    private static let traditionalChinese: [L10nKey: String] = [
        .productName: "CodexFold",
        .statusHealthy: "運作正常",
        .statusRecovering: "正在復原",
        .statusAttention: "需要處理",
        .statusUnavailable: "狀態無法使用",
        .statusUnknown: "等待狀態",
        .statusSummaryHealthy: "CodexFold 運作正常",
        .statusSummaryRecovering: "檔案存取正在自動復原",
        .statusSummaryAttention: "檔案存取需要處理",
        .statusSummaryUnavailable: "尚未連接到 CodexFold 檔案服務",
        .openDashboard: "開啟 CodexFold",
        .checkStatus: "立即檢查",
        .exportDiagnostics: "匯出診斷",
        .copyDiagnostics: "複製診斷",
        .revealStatusFiles: "在 Finder 中顯示狀態檔案",
        .quit: "結束 CodexFold 選單列",
        .overview: "概覽",
        .components: "運作狀態",
        .updated: "更新時間",
        .neverUpdated: "尚未收到狀態",
        .noComponents: "正在等待 CodexFold 回報狀態",
        .unconnectedTitle: "檔案服務尚未連接",
        .unconnectedDetail: "完成 CodexFold 安裝並啟動服務後，這裡會顯示節省空間和運作狀態。",
        .loginItemNotConfigured: "尚未開啟登入後自動啟動。完成安裝後可隨系統登入運作。",
        .loginItemRequiresApproval: "請在「系統設定 > 一般 > 登入項目」中允許 CodexFold 登入後啟動。",
        .loginItemUnavailable: "CodexFold 暫時無法設定登入後啟動，檔案服務不受影響。",
        .incidentMonitorNotConfigured: "尚未開啟背景故障提醒。完成安裝後可在 CodexFold 未開啟時繼續提醒。",
        .incidentMonitorRequiresApproval: "請在「系統設定 > 一般 > 登入項目」中允許 CodexFold 故障提醒；背景檔案服務不會因此重新啟動。",
        .incidentMonitorUnavailable: "CodexFold 暫時無法常駐顯示故障提醒；選單列仍會在運作時顯示故障視窗。",
        .administratorRecoveryRecommendation: "如需管理員級修復，請只從你明確點擊的 CodexFold 操作進入系統授權；CodexFold 不會自動要求密碼或使用 sudo。",
        .administratorCapabilityMemberDetail: "本機授權能力：目前帳戶屬於 admin 群組；未探測 sudo 快取；自動提權已停用；任何系統授權都必須由使用者明確發起。",
        .administratorCapabilityNotMemberDetail: "本機授權能力：目前帳戶不屬於 admin 群組；未探測 sudo 快取；自動提權已停用；任何系統授權都必須由使用者明確發起。",
        .administratorCapabilityUnknownDetail: "本機授權能力：無法唯讀確認目前帳戶是否屬於 admin 群組；未探測 sudo 快取；自動提權已停用；任何系統授權都必須由使用者明確發起。",
        .incidentTitle: "CodexFold 需要處理",
        .incidentRecoveredTitle: "檔案存取已復原",
        .incidentRecoveredBody: "Codex 可以繼續正常使用",
        .reason: "發生了什麼",
        .impact: "影響",
        .recommendedActions: "你現在可以這樣做",
        .technicalDetails: "技術詳情",
        .close: "關閉",
        .save: "儲存",
        .diagnosticsSaved: "診斷已匯出",
        .diagnosticsSaveFailed: "診斷匯出失敗",
        .stale: "狀態已過期",
        .waitingForStatus: "等待背景狀態檔案",
        .managedSessions: "已管理工作階段",
        .logicalBytes: "邏輯資料",
        .physicalBytes: "實際佔用",
        .spaceSaved: "已節省",
        .savingsRate: "節省比例",
        .sessionDataSize: "工作階段原始大小",
        .diskUsage: "目前磁碟佔用",
        .storageHistory: "空間變化",
        .ioActivity: "讀寫速度",
        .readRate: "讀取",
        .writeRate: "寫入",
        .noActivityHistory: "正在收集讀寫速度，幾分鐘後會顯示趨勢。",
        .recentIncidents: "最近故障",
        .noStorageHistory: "正在收集空間變化，幾分鐘後會顯示趨勢。",
        .noRecentIncidents: "最近沒有記錄到持續 10 秒的故障。",
        .rangeOneHour: "1 小時",
        .rangeOneDay: "24 小時",
        .rangeSevenDays: "7 天",
        .rangeThirtyDays: "30 天",
        .originalSize: "原始大小",
        .currentUsage: "磁碟佔用",
        .incidentOngoing: "仍在發生",
        .incidentRecovered: "已復原",
        .lastUpdatedFormat: "%@ 更新",
        .unknownReason: "CodexFold 前端持續無法完成健康檢查。",
        .unknownImpact: "Codex 可能持續重新連線，開啟或寫入工作階段可能失敗。",
        .retryRecommendation: "等待自動復原，避免強制結束正在寫入的 Codex。",
        .openDashboardRecommendation: "開啟 CodexFold 查看元件狀態並匯出診斷。",
        .daemonStatusChannelSummary: "正在復原背景服務監測",
        .daemonStatusChannelRecoveredSummary: "背景服務監測已復原",
        .daemonStatusChannelReason: "CodexFold 已持續無法取得可信且持續更新的背景服務狀態。",
        .daemonStatusChannelImpact: "Codex 會繼續運作，CodexFold 不會操作或重新啟動 Codex；在背景服務復原前，工作階段開啟或寫入可能等待或失敗。",
        .daemonStatusChannelWaitRecommendation: "保持 Codex 和 CodexFold 運作，等待背景服務自動復原。",
        .daemonStatusChannelMissingDetail: "尚未收到可安全讀取的背景服務狀態報告。",
        .daemonStatusChannelInvalidDetail: "收到的背景服務狀態報告不完整或無法安全讀取。",
        .daemonStatusChannelContinuityInvalidDetail: "背景服務狀態與耐久復原基線不一致，暫時無法證明已經復原。",
        .daemonStatusChannelStaleDetail: "背景服務狀態報告已停止更新。",
        .managedStatusChannelSummary: "正在復原工作階段狀態監測",
        .managedStatusChannelRecoveredSummary: "工作階段狀態監測已復原",
        .managedStatusChannelReason: "CodexFold 已持續無法取得可信的工作階段管理狀態。",
        .managedStatusChannelImpact: "Codex 會繼續運作，現有檔案服務不會因此重新啟動，但 CodexFold 暫時無法確認工作階段管理是否正常。",
        .managedStatusChannelWaitRecommendation: "保持 Codex 和 CodexFold 運作，等待狀態監測自動復原。",
        .managedStatusChannelMissingDetail: "尚未收到工作階段管理狀態報告。",
        .managedStatusChannelInvalidDetail: "收到的工作階段管理狀態報告不完整或無法安全讀取。",
        .managedStatusChannelContinuityInvalidDetail: "工作階段管理狀態與耐久復原基線不一致，暫時無法證明已經復原。",
        .managedStatusChannelStaleDetail: "工作階段管理狀態報告已停止更新。",
        .supervisorStatusChannelSummary: "正在復原檔案服務守護監測",
        .supervisorStatusChannelRecoveredSummary: "檔案服務守護監測已復原",
        .supervisorStatusChannelReason: "CodexFold 已持續無法取得可信的檔案服務守護狀態。",
        .supervisorStatusChannelImpact: "Codex 會繼續運作，CodexFold 不會結束、重新啟動或操作 Codex，但目前無法確認檔案服務守護是否仍在工作。",
        .supervisorStatusChannelWaitRecommendation: "保持 Codex 和 CodexFold 運作，等待守護監測自動復原。",
        .supervisorStatusChannelMissingDetail: "尚未收到檔案服務守護狀態報告。",
        .supervisorStatusChannelInvalidDetail: "收到的檔案服務守護狀態報告不完整或無法安全讀取。",
        .supervisorStatusChannelContinuityInvalidDetail: "檔案服務守護狀態與耐久復原基線不一致，暫時無法證明已經復原。",
        .supervisorStatusChannelStaleDetail: "檔案服務守護狀態報告已停止更新。",
        .supervisorRecoveryReason: "CodexFold 檔案服務守護持續無法確認前端與後台連線都已復原。",
        .supervisorFailureImpact: "Codex 會繼續運作，掛載點維持存在；在自動復原完成前，工作階段開啟或寫入可能等待或失敗。",
        .frontend: "檔案存取",
        .managed: "工作階段管理",
        .supervisor: "檔案服務守護",
        .daemon: "背景服務",
        .storage: "儲存",
        .autoFold: "自動折疊",
        .autoFoldDetail: "關掉後不再自動折。正在折的這一條會在目前步驟結束後停下，不用結束 Codex。",
        .autoFoldCheckInterval: "多久檢查一次",
        .autoFoldIdleFor: "閒置多久才折",
        .autoFoldScope: "折哪些",
        .autoFoldBatch: "每轮折多少",
        .autoFoldBatchAutomatic: "自动",
        .autoFoldBatchCountFormat: "%d 个会话",
        .autoFoldScopeArchived: "只已封存",
        .autoFoldScopeArchivedAndIdle: "已封存和閒置中的",
        .autoFoldProgressFoldedFormat: "已折疊 %d · 還剩 %d",
        .autoFoldProgressFoldedOnlyFormat: "已折疊 %d",
        .autoFoldChecking: "正在檢查閒置對話",
        .autoFoldFolding: "正在折疊",
        .autoFoldReclaiming: "正在校驗並釋放空間",
        .autoFoldReclaimWaiting: "等待使用中的任務釋放檔案後繼續回收",
        .autoFoldOff: "已關閉",
        .autoFoldWaitingForCheck: "打開後會按設定檢查",
        .autoFoldRetry: "這一輪沒折成，會按設定再試",
        .autoFoldConfigurationError: "自動折疊設定有問題，已暫停，請修復後再試。",
        .autoFoldCheckSoon: "即將再檢查",
        .autoFoldCheckUnderTwoMinutes: "不到 2 分鐘後再檢查",
        .autoFoldCheckMinutesFormat: "約 %d 分鐘後再檢查",
        .autoFoldCheckHoursFormat: "約 %d 小時後再檢查",
        .autoFoldSaveFailed: "自動折疊設定沒能儲存，請再試一次。",
        .durationThirtyMinutes: "30 分鐘",
        .durationTwoHours: "2 小時",
        .durationSixHours: "6 小時",
        .durationOneDay: "1 天",
    ]

    private static let english: [L10nKey: String] = [
        .productName: "CodexFold",
        .statusHealthy: "Running normally",
        .statusRecovering: "Recovering",
        .statusAttention: "Needs attention",
        .statusUnavailable: "Status unavailable",
        .statusUnknown: "Waiting for status",
        .statusSummaryHealthy: "CodexFold is running normally",
        .statusSummaryRecovering: "File access is recovering automatically",
        .statusSummaryAttention: "File access needs attention",
        .statusSummaryUnavailable: "Not connected to the CodexFold file service",
        .openDashboard: "Open CodexFold",
        .checkStatus: "Check now",
        .exportDiagnostics: "Export diagnostics",
        .copyDiagnostics: "Copy diagnostics",
        .revealStatusFiles: "Show status files in Finder",
        .quit: "Quit CodexFold menu bar",
        .overview: "Overview",
        .components: "Service status",
        .updated: "Updated",
        .neverUpdated: "No status received yet",
        .noComponents: "Waiting for CodexFold to report status",
        .unconnectedTitle: "File service not connected",
        .unconnectedDetail: "After CodexFold is installed and the service starts, space savings and service status will appear here.",
        .loginItemNotConfigured: "Launch at login is off. A completed install can keep CodexFold available after sign-in.",
        .loginItemRequiresApproval: "Allow CodexFold under System Settings > General > Login Items to start it after login.",
        .loginItemUnavailable: "CodexFold could not configure login launch. File service is unaffected.",
        .incidentMonitorNotConfigured: "Background alerts are off. A completed install can keep alerts available while the app is closed.",
        .incidentMonitorRequiresApproval: "Allow CodexFold incident alerts under System Settings > General > Login Items. The background file service will not be restarted.",
        .incidentMonitorUnavailable: "CodexFold could not keep incident alerts resident. The menu-bar app will still show incident windows while it is running.",
        .administratorRecoveryRecommendation: "If an administrator-level repair is needed, enter system authorization only from a CodexFold action you explicitly choose. CodexFold never requests a password or uses sudo automatically.",
        .administratorCapabilityMemberDetail: "Local authorization capability: this account belongs to the admin group; the sudo cache was not probed; automatic elevation is disabled; every system authorization requires an explicit user action.",
        .administratorCapabilityNotMemberDetail: "Local authorization capability: this account does not belong to the admin group; the sudo cache was not probed; automatic elevation is disabled; every system authorization requires an explicit user action.",
        .administratorCapabilityUnknownDetail: "Local authorization capability: admin-group membership could not be confirmed read-only; the sudo cache was not probed; automatic elevation is disabled; every system authorization requires an explicit user action.",
        .incidentTitle: "CodexFold needs attention",
        .incidentRecoveredTitle: "File access restored",
        .incidentRecoveredBody: "Codex is ready to use",
        .reason: "What happened",
        .impact: "Impact",
        .recommendedActions: "What you can do",
        .technicalDetails: "Technical details",
        .close: "Close",
        .save: "Save",
        .diagnosticsSaved: "Diagnostics exported",
        .diagnosticsSaveFailed: "Diagnostics export failed",
        .stale: "Status is stale",
        .waitingForStatus: "Waiting for background status files",
        .managedSessions: "Managed sessions",
        .logicalBytes: "Logical data",
        .physicalBytes: "Physical usage",
        .spaceSaved: "Space saved",
        .savingsRate: "Savings",
        .sessionDataSize: "Original session size",
        .diskUsage: "Current disk usage",
        .storageHistory: "Storage over time",
        .ioActivity: "Read and write speed",
        .readRate: "Read",
        .writeRate: "Write",
        .noActivityHistory: "Collecting read and write speed. A trend will appear in a few minutes.",
        .recentIncidents: "Recent incidents",
        .noStorageHistory: "Collecting storage history. A trend will appear in a few minutes.",
        .noRecentIncidents: "No failure lasting 10 seconds has been recorded recently.",
        .rangeOneHour: "1 hour",
        .rangeOneDay: "24 hours",
        .rangeSevenDays: "7 days",
        .rangeThirtyDays: "30 days",
        .originalSize: "Original size",
        .currentUsage: "Disk usage",
        .incidentOngoing: "Ongoing",
        .incidentRecovered: "Recovered",
        .lastUpdatedFormat: "Updated %@",
        .unknownReason: "The CodexFold frontend has continuously failed its health check.",
        .unknownImpact: "Codex may keep reconnecting, and opening or writing sessions may fail.",
        .retryRecommendation: "Allow automatic recovery and avoid force-quitting Codex while it is writing.",
        .openDashboardRecommendation: "Open CodexFold to review component status and export diagnostics.",
        .daemonStatusChannelSummary: "Recovering backend status monitoring",
        .daemonStatusChannelRecoveredSummary: "Backend status monitoring recovered",
        .daemonStatusChannelReason: "CodexFold has continuously been unable to obtain trustworthy, advancing backend status.",
        .daemonStatusChannelImpact: "Codex remains running, and CodexFold does not operate or restart Codex; opening or writing sessions may wait or fail until the backend recovers.",
        .daemonStatusChannelWaitRecommendation: "Keep Codex and CodexFold running while the backend recovers automatically.",
        .daemonStatusChannelMissingDetail: "No safely readable backend status report has been received.",
        .daemonStatusChannelInvalidDetail: "The backend status report is incomplete or cannot be read safely.",
        .daemonStatusChannelContinuityInvalidDetail: "The backend status does not match its durable recovery baseline, so recovery cannot yet be proven.",
        .daemonStatusChannelStaleDetail: "The backend status report has stopped updating.",
        .managedStatusChannelSummary: "Recovering session status monitoring",
        .managedStatusChannelRecoveredSummary: "Session status monitoring recovered",
        .managedStatusChannelReason: "CodexFold has continuously been unable to obtain trustworthy session-management status.",
        .managedStatusChannelImpact: "Codex remains running and the existing file service is not restarted, but CodexFold cannot currently confirm that session management is healthy.",
        .managedStatusChannelWaitRecommendation: "Keep Codex and CodexFold running while status monitoring recovers automatically.",
        .managedStatusChannelMissingDetail: "No session-management status report has been received.",
        .managedStatusChannelInvalidDetail: "The session-management status report is incomplete or cannot be read safely.",
        .managedStatusChannelContinuityInvalidDetail: "The session-management status does not match its durable recovery baseline, so recovery cannot yet be proven.",
        .managedStatusChannelStaleDetail: "The session-management status report has stopped updating.",
        .supervisorStatusChannelSummary: "Recovering file-service supervision monitoring",
        .supervisorStatusChannelRecoveredSummary: "File-service supervision monitoring recovered",
        .supervisorStatusChannelReason: "CodexFold has continuously been unable to obtain trustworthy file-service supervisor status.",
        .supervisorStatusChannelImpact: "Codex remains running, and CodexFold does not quit, restart, or operate Codex, but it cannot currently confirm that file-service supervision is active.",
        .supervisorStatusChannelWaitRecommendation: "Keep Codex and CodexFold running while supervisor monitoring recovers automatically.",
        .supervisorStatusChannelMissingDetail: "No file-service supervisor status report has been received.",
        .supervisorStatusChannelInvalidDetail: "The file-service supervisor status report is incomplete or cannot be read safely.",
        .supervisorStatusChannelContinuityInvalidDetail: "The file-service supervisor status does not match its durable recovery baseline, so recovery cannot yet be proven.",
        .supervisorStatusChannelStaleDetail: "The file-service supervisor status report has stopped updating.",
        .supervisorRecoveryReason: "The CodexFold file-service supervisor still cannot confirm that both the frontend and backend connection have recovered.",
        .supervisorFailureImpact: "Codex remains running and the mount stays present; opening or writing sessions may wait or fail until automatic recovery completes.",
        .frontend: "File access",
        .managed: "Session management",
        .supervisor: "File service monitor",
        .daemon: "Background service",
        .storage: "Storage",
        .autoFold: "Auto fold",
        .autoFoldDetail: "Turning this off stops future folding. A fold already in progress finishes its current step. You do not need to quit Codex.",
        .autoFoldCheckInterval: "How often to check",
        .autoFoldIdleFor: "How long idle before folding",
        .autoFoldScope: "What to fold",
        .autoFoldBatch: "How many per pass",
        .autoFoldBatchAutomatic: "Automatic",
        .autoFoldBatchCountFormat: "%d sessions",
        .autoFoldScopeArchived: "Archived only",
        .autoFoldScopeArchivedAndIdle: "Archived and idle",
        .autoFoldProgressFoldedFormat: "Folded %d · %d remaining",
        .autoFoldProgressFoldedOnlyFormat: "Folded %d",
        .autoFoldChecking: "Checking idle conversations",
        .autoFoldFolding: "Folding now",
        .autoFoldReclaiming: "Verifying and freeing space",
        .autoFoldReclaimWaiting: "Waiting for active tasks to release files before cleanup",
        .autoFoldOff: "Off",
        .autoFoldWaitingForCheck: "Will check on the schedule you set",
        .autoFoldRetry: "This pass did not fold. It will try again on schedule.",
        .autoFoldConfigurationError: "Auto-fold settings are invalid, so folding is paused until they are fixed.",
        .autoFoldCheckSoon: "Checking again shortly",
        .autoFoldCheckUnderTwoMinutes: "Checking again in under 2 minutes",
        .autoFoldCheckMinutesFormat: "Checking again in about %d minutes",
        .autoFoldCheckHoursFormat: "Checking again in about %d hours",
        .autoFoldSaveFailed: "Could not save auto-fold settings. Try again.",
        .durationThirtyMinutes: "30 minutes",
        .durationTwoHours: "2 hours",
        .durationSixHours: "6 hours",
        .durationOneDay: "1 day",
    ]
}
