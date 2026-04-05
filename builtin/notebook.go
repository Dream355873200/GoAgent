// Package builtin 提供内置工具。
package builtin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Dream355873200/GoAgent"
)

// NotebookEditInput 是 Notebook 编辑的输入。
type NotebookEditInput struct {
	Path     string `json:"path" desc:"Notebook 文件路径 (.ipynb)" required:"true"`
	CellIdx  int    `json:"cell_idx" desc:"要编辑的单元格索引（从 0 开始）" required:"true"`
	NewCode  string `json:"new_code" desc:"新的代码内容" required:"true"`
	CellType string `json:"cell_type,omitempty" desc:"单元格类型：code 或 markdown，默认 code"`
}

// NotebookEditTool 返回 Jupyter Notebook 编辑工具定义。
func NotebookEditTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "编辑 Jupyter Notebook 中的代码单元格。支持修改现有代码或添加新单元格。",
		Input:       NotebookEditInput{},
		Permission:  goagent.Normal,
		Execute: func(ctx goagent.Context, in NotebookEditInput) (string, error) {
			return executeNotebookEdit(in)
		},
	}
}

func executeNotebookEdit(in NotebookEditInput) (string, error) {
	if in.Path == "" {
		return "", fmt.Errorf("path 不能为空")
	}

	if filepath.Ext(in.Path) != ".ipynb" {
		return "", fmt.Errorf("不是 .ipynb 文件")
	}

	data, err := os.ReadFile(in.Path)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}

	// 解析 JSON
	var notebook map[string]any
	if err := json.Unmarshal(data, &notebook); err != nil {
		return "", fmt.Errorf("JSON 解析失败: %w", err)
	}

	cells, ok := notebook["cells"].([]any)
	if !ok {
		return "", fmt.Errorf("无法读取 cells")
	}

	cellType := in.CellType
	if cellType == "" {
		cellType = "code"
	}

	// 检查单元格索引
	if in.CellIdx < 0 || in.CellIdx >= len(cells) {
		// 如果索引超出范围，创建新单元格
		newCell := createNotebookCell(cellType, in.NewCode)
		notebook["cells"] = append(cells, newCell)
	} else {
		// 编辑现有单元格
		cell, ok := cells[in.CellIdx].(map[string]any)
		if !ok {
			return "", fmt.Errorf("无法解析单元格 %d", in.CellIdx)
		}

		switch cellType {
		case "code":
			cell["cell_type"] = "code"
			if source, ok := cell["source"].([]any); ok {
				// 如果 source 是数组，替换第一行
				if len(source) > 0 {
					source[0] = in.NewCode
				} else {
					cell["source"] = []any{in.NewCode}
				}
			} else if source, ok := cell["source"].(string); ok {
				cell["source"] = []any{source + "\n" + in.NewCode}
			} else {
				cell["source"] = []any{in.NewCode}
			}
		case "markdown":
			cell["cell_type"] = "markdown"
			cell["source"] = []any{in.NewCode}
		}
	}

	// 写回文件
	output, err := json.MarshalIndent(notebook, "", "  ")
	if err != nil {
		return "", fmt.Errorf("JSON 序列化失败: %w", err)
	}

	if err := os.WriteFile(in.Path, output, 0644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	return fmt.Sprintf("已编辑 Notebook %s 的单元格 %d", filepath.Base(in.Path), in.CellIdx), nil
}

// createNotebookCell 创建一个新的 notebook 单元格。
func createNotebookCell(cellType, source string) map[string]any {
	cell := map[string]any{
		"cell_type": cellType,
		"metadata":  map[string]any{},
		"source":    []any{source},
	}

	if cellType == "code" {
		cell["outputs"] = []any{}
		cell["execution_count"] = nil
	}

	return cell
}

// NotebookAddCellInput 是添加单元格的输入。
type NotebookAddCellInput struct {
	Path     string `json:"path" desc:"Notebook 文件路径" required:"true"`
	Position int    `json:"position,omitempty" desc:"插入位置（默认追加到末尾）"`
	Code     string `json:"code" desc:"代码内容" required:"true"`
	CellType string `json:"cell_type,omitempty" desc:"单元格类型：code 或 markdown"`
}

// NotebookAddCellTool 返回添加单元格的工具定义。
func NotebookAddCellTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "向 Jupyter Notebook 添加新的代码或 markdown 单元格。",
		Input:       NotebookAddCellInput{},
		Permission:  goagent.Normal,
		Execute: func(ctx goagent.Context, in NotebookAddCellInput) (string, error) {
			return executeNotebookAddCell(in)
		},
	}
}

func executeNotebookAddCell(in NotebookAddCellInput) (string, error) {
	if in.Path == "" {
		return "", fmt.Errorf("path 不能为空")
	}

	if filepath.Ext(in.Path) != ".ipynb" {
		return "", fmt.Errorf("不是 .ipynb 文件")
	}

	data, err := os.ReadFile(in.Path)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}

	var notebook map[string]any
	if err := json.Unmarshal(data, &notebook); err != nil {
		return "", fmt.Errorf("JSON 解析失败: %w", err)
	}

	cells, ok := notebook["cells"].([]any)
	if !ok {
		return "", fmt.Errorf("无法读取 cells")
	}

	cellType := in.CellType
	if cellType == "" {
		cellType = "code"
	}

	newCell := createNotebookCell(cellType, in.Code)

	pos := in.Position
	if pos < 0 || pos > len(cells) {
		pos = len(cells)
	}

	// 插入单元格
	if pos == len(cells) {
		cells = append(cells, newCell)
	} else {
		cells = append(cells[:pos], append([]any{newCell}, cells[pos:]...)...)
	}

	notebook["cells"] = cells

	output, err := json.MarshalIndent(notebook, "", "  ")
	if err != nil {
		return "", fmt.Errorf("JSON 序列化失败: %w", err)
	}

	if err := os.WriteFile(in.Path, output, 0644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	return fmt.Sprintf("已添加单元格到 Notebook %s (位置: %d)", filepath.Base(in.Path), pos), nil
}

// NotebookRunCellInput 是运行单元格的输入。
type NotebookRunCellInput struct {
	Path    string `json:"path" desc:"Notebook 文件路径" required:"true"`
	CellIdx int    `json:"cell_idx" desc:"要运行的单元格索引" required:"true"`
}

// NotebookRunCellTool 返回运行单元格的工具定义。
// 注意：此工具只是标记单元格需要运行，实际执行由内核完成。
func NotebookRunCellTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "标记 Notebook 中的单元格为待运行状态。实际执行需要 Jupyter 内核。",
		Input:       NotebookRunCellInput{},
		Permission:  goagent.Normal,
		Execute: func(ctx goagent.Context, in NotebookRunCellInput) (string, error) {
			return executeNotebookRunCell(in)
		},
	}
}

func executeNotebookRunCell(in NotebookRunCellInput) (string, error) {
	if in.Path == "" {
		return "", fmt.Errorf("path 不能为空")
	}

	data, err := os.ReadFile(in.Path)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}

	var notebook map[string]any
	if err := json.Unmarshal(data, &notebook); err != nil {
		return "", fmt.Errorf("JSON 解析失败: %w", err)
	}

	cells, ok := notebook["cells"].([]any)
	if !ok {
		return "", fmt.Errorf("无法读取 cells")
	}

	if in.CellIdx < 0 || in.CellIdx >= len(cells) {
		return "", fmt.Errorf("单元格索引 %d 超出范围", in.CellIdx)
	}

	cell, ok := cells[in.CellIdx].(map[string]any)
	if !ok {
		return "", fmt.Errorf("无法解析单元格")
	}

	// 提取代码
	var code string
	if source, ok := cell["source"].([]any); ok {
		for _, s := range source {
			if str, ok := s.(string); ok {
				code += str
			}
		}
	} else if source, ok := cell["source"].(string); ok {
		code = source
	}

	// 清空输出
	cell["outputs"] = []any{}
	cell["execution_count"] = nil

	output, err := json.MarshalIndent(notebook, "", "  ")
	if err != nil {
		return "", fmt.Errorf("JSON 序列化失败: %w", err)
	}

	if err := os.WriteFile(in.Path, output, 0644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	if len(code) > 100 {
		code = code[:100] + "..."
	}

	return fmt.Sprintf("已标记单元格 %d 待运行:\n%s", in.CellIdx, code), nil
}
