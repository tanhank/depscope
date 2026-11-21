package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/janghotan/depscope/internal/i18n"
)

// handleMouse 处理鼠标滚轮滚动、悬停高亮与左键点击交互。
func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// 1. 处理鼠标滚轮滚动
	if msg.Button == tea.MouseButtonWheelUp {
		m.scrollBy(-3)
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelDown {
		m.scrollBy(3)
		return m, nil
	}

	// 2. 处理鼠标悬停移动：动态更新当前焦点并播放焦点动画
	if msg.Action == tea.MouseActionMotion {
		return m.handleMouseHover(msg)
	}

	// 仅响应鼠标左键按下事件
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	// 3. 点击顶部页眉 (Y <= 1) 返回主菜单或上级视图
	if msg.Y <= 1 {
		switch m.screen {
		case screenReport:
			if m.reportView != viewOverview {
				m.reportView = viewOverview
				m.scroll = 0
				return m, m.startFocus()
			}
			return m, m.returnToMenu()
		case screenPackageForm, screenProjectForm, screenCompareForm, screenInventoryConfirm, screenDemoPicker, screenSettings, screenComparison:
			return m, m.returnToMenu()
		case screenHelp:
			m.screen = m.helpReturn
			m.scroll = 0
			return m, nil
		case screenError:
			return m, m.returnToMenu()
		}
		return m, nil
	}

	// 4. 点击底部提示栏 (Y >= m.height - 1)
	if msg.Y >= m.height-1 {
		switch m.screen {
		case screenReport:
			m.cycleReportView(1)
			return m, m.startFocus()
		case screenHelp:
			m.screen = m.helpReturn
			m.scroll = 0
			return m, nil
		case screenError:
			return m, m.returnToMenu()
		}
	}

	// 5. 计算内容区域相对行号 (页眉固定占 2 行)
	contentY := msg.Y - 2 + m.scroll
	if contentY < 0 {
		return m, nil
	}

	switch m.screen {
	case screenMenu:
		return m.handleMenuMouseClick(contentY, msg.X)
	case screenDemoPicker:
		return m.handleDemoMouseClick(contentY)
	case screenSettings:
		return m.handleSettingsMouseClick(contentY)
	case screenPackageForm, screenProjectForm, screenCompareForm:
		return m.handleFormMouseClick(contentY)
	case screenInventoryConfirm:
		return m, m.startScan(m.beginInventoryScan())
	case screenReport:
		return m.handleReportMouseClick(contentY)
	case screenHelp:
		m.screen = m.helpReturn
		m.scroll = 0
		return m, nil
	case screenError:
		return m, m.returnToMenu()
	}

	return m, nil
}

// handleMouseHover 处理鼠标在各界面悬停时的焦点高亮与动态反馈。
func (m *Model) handleMouseHover(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Y <= 1 || msg.Y >= m.height-1 {
		return m, nil
	}
	contentY := msg.Y - 2 + m.scroll
	if contentY < 0 {
		return m, nil
	}

	switch m.screen {
	case screenMenu:
		idx := m.menuIndexAt(contentY, msg.X)
		if idx >= 0 && idx < len(menuEntries) && m.menuIndex != idx {
			m.menuIndex = idx
			return m, m.startFocus()
		}
	case screenSettings:
		row := m.settingsRowAt(contentY)
		if row >= 0 && row < 5 && m.settingsAt != row {
			m.settingsAt = row
			return m, m.startFocus()
		}
	case screenDemoPicker:
		idx := m.demoIndexAt(contentY)
		if idx >= 0 && idx < len(m.demos) && m.demoAt != idx {
			m.demoAt = idx
			return m, m.startFocus()
		}
	case screenReport:
		if m.reportView == viewOverview && m.document != nil && len(m.document.Findings) > 0 {
			findingBaseY := 8
			if contentY >= findingBaseY {
				idx := (contentY - findingBaseY) / 2
				if idx >= 0 && idx < len(m.document.Findings) && m.findingIndex != idx {
					m.findingIndex = idx
					return m, m.startFocus()
				}
			}
		}
	}

	return m, nil
}

// menuIndexAt 计算主菜单中内容坐标对应的菜单项索引 (0..5)。若未落在菜单项区域则返回 -1。
func (m *Model) menuIndexAt(contentY int, contentX int) int {
	width := m.contentWidth()
	height := m.contentHeight()
	size := selectLogoSize(width, height)
	logoLines := len(strings.Split(m.brandedLogo(size), "\n"))
	startMenuY := logoLines + 2 + 2

	if contentY < startMenuY || contentY >= startMenuY+len(menuEntries) {
		return -1
	}

	layout := layoutFor(width, height)
	if layout == layoutTwoColumn {
		leftWidth, _ := columnWidths(width)
		if contentX > leftWidth+2 {
			return -1
		}
	}

	return contentY - startMenuY
}

// handleMenuMouseClick 处理主菜单界面的点击。
func (m *Model) handleMenuMouseClick(contentY int, contentX int) (tea.Model, tea.Cmd) {
	idx := m.menuIndexAt(contentY, contentX)
	if idx >= 0 && idx < len(menuEntries) {
		m.menuIndex = idx
		return m, m.openScreen(menuEntries[idx].target)
	}
	return m, nil
}

// demoIndexAt 计算内容相对行 contentY 对应的演示场景索引。若未落在演示项区域则返回 -1。
func (m *Model) demoIndexAt(contentY int) int {
	if len(m.demos) == 0 {
		return -1
	}
	width := m.contentWidth()
	introLines := len(wrapPlain(m.text(i18n.KeyDemoIntro), width))
	startDemoY := 1 + introLines + 1

	if contentY < startDemoY {
		return -1
	}

	currentY := startDemoY
	for index, scenario := range m.demos {
		iconText := m.demoIconText(scenario.ID)
		indent := strings.Repeat(" ", 6+displayWidth(iconText))
		descLines := len(wrapPlain(m.demoDescription(scenario), width-displayWidth(indent)))
		scenarioHeight := 1 + descLines
		if contentY >= currentY && contentY < currentY+scenarioHeight {
			return index
		}
		currentY += scenarioHeight
	}
	return -1
}

// handleDemoMouseClick 处理演示场景选择界面的点击。
func (m *Model) handleDemoMouseClick(contentY int) (tea.Model, tea.Cmd) {
	idx := m.demoIndexAt(contentY)
	if idx >= 0 && idx < len(m.demos) {
		m.demoAt = idx
		return m, m.startScan(m.beginDemoScan(m.demos[idx].ID))
	}
	return m, nil
}

// settingsRowAt 计算内容相对行 contentY 对应的设置项索引 (0..4)。若未落在设置项区域则返回 -1。
func (m *Model) settingsRowAt(contentY int) int {
	width := m.contentWidth()
	introLines := len(wrapPlain(m.text(i18n.KeySettingsIntro), width))
	startSettingsY := 1 + introLines + 1

	if contentY < startSettingsY {
		return -1
	}

	rows := []struct {
		label string
		value string
	}{
		{m.text(i18n.KeySettingsLanguageLabel), m.language.NativeName()},
		{m.text(i18n.KeySettingsEcosystemLabel), string(m.ecosystem)},
		{m.text(i18n.KeySettingsColorLabel), m.colorDescription()},
		{m.text(i18n.KeySettingsAnimationLabel), m.animationDescription()},
		{"", m.text(i18n.KeySettingsBack)},
	}

	currentY := startSettingsY
	for index, row := range rows {
		rowHeight := 1
		if row.label != "" {
			marker := "  "
			iconText := "   "
			labelText := padPlain(row.label, 20)
			prefix := marker + iconText + labelText
			valWidth := width - displayWidth(prefix)
			if valWidth < 1 {
				valWidth = 1
			}
			lines := len(wrapPlain(row.value, valWidth))
			if lines > 1 {
				rowHeight = lines
			}
		}

		if contentY >= currentY && contentY < currentY+rowHeight {
			return index
		}
		currentY += rowHeight
	}
	return -1
}

// handleSettingsMouseClick 处理设置界面的点击。
func (m *Model) handleSettingsMouseClick(contentY int) (tea.Model, tea.Cmd) {
	row := m.settingsRowAt(contentY)
	if row >= 0 && row < 5 {
		m.settingsAt = row
		return m, m.changeSetting(false)
	}
	return m, nil
}

// handleFormMouseClick 处理表单界面的点击。
func (m *Model) handleFormMouseClick(contentY int) (tea.Model, tea.Cmd) {
	// 如果是双输入框的对比表单
	if m.screen == screenCompareForm && len(m.formFields) >= 2 {
		if contentY >= 6 && contentY <= 7 {
			m.formIndex = 0
			return m, nil
		}
		if contentY >= 8 && contentY <= 10 {
			m.formIndex = 1
			return m, nil
		}
		if contentY > 10 {
			return m.submitForm()
		}
	} else {
		// 单输入框表单
		if contentY > 6 {
			return m.submitForm()
		}
	}
	return m, nil
}

// handleReportMouseClick 处理扫描报告界面的点击。
func (m *Model) handleReportMouseClick(contentY int) (tea.Model, tea.Cmd) {
	if m.document == nil {
		return m, nil
	}

	// 点击顶部区域 (前 3 行) 循环切换报告分节视图
	if contentY <= 3 {
		m.cycleReportView(1)
		return m, m.startFocus()
	}

	// 在概览视图中，若点击漏洞列表区域，打开对应漏洞详情
	if m.reportView == viewOverview && len(m.document.Findings) > 0 {
		findingBaseY := 8 // 概览中漏洞条目通常起始位置
		if contentY >= findingBaseY {
			idx := (contentY - findingBaseY) / 2
			if idx >= 0 && idx < len(m.document.Findings) {
				m.findingIndex = idx
				m.reportView = viewFinding
				m.scroll = 0
				return m, m.startFocus()
			}
		}
	} else if m.reportView == viewFinding {
		// 在详情页点击可返回概览
		if contentY <= 5 {
			m.reportView = viewOverview
			m.scroll = 0
			return m, m.startFocus()
		}
	}

	return m, nil
}
