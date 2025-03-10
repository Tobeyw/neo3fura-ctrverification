package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/nspcc-dev/neo-go/pkg/smartcontract/nef"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/cors"
	"github.com/tidwall/gjson"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"gopkg.in/yaml.v3"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"regexp"
)

// 定义主网和测试网节点常量，之后根据部署主网测试网选择响应的结点
const RPCNODEMAIN = "https://neofura.ngd.network"

const RPCNODETEST = "https://testneofura.ngd.network:444"

const RPCNODETESTMAGNET = "https://testmagnet.ngd.network"

//定义主网和测试往数据库结构
type Config struct {
	Database_main struct {
		Host     string `yaml:"host"`
		Port     string `yaml:"port"`
		User     string `yaml:"user"`
		Pass     string `yaml:"pass"`
		Database string `yaml:"database"`
		DBName   string `yaml:"dbname"`
	} `yaml:"database_main"`
	Database_test struct {
		Host     string `yaml:"host"`
		Port     string `yaml:"port"`
		User     string `yaml:"user"`
		Pass     string `yaml:"pass"`
		Database string `yaml:"database"`
		DBName   string `yaml:"dbname"`
	} `yaml:"database_test"`
	Database_testmagnet struct {
		Host     string `yaml:"host"`
		Port     string `yaml:"port"`
		User     string `yaml:"user"`
		Pass     string `yaml:"pass"`
		Database string `yaml:"database"`
		DBName   string `yaml:"dbname"`
	} `yaml:"database_testmagnet"`
}

//定义http应答返回格式
type jsonResult struct {
	Code int
	Msg  string
}

//定义插入VerifiedContract表的数据格式, 记录被验证的合约
type insertVerifiedContract struct {
	Hash          string
	Id            int
	Updatecounter int
}

//定义插入ContractSourceCode表的数据格式，记录被验证的合约源代码
type insertContractSourceCode struct {
	Hash          string
	Updatecounter int
	FileName      string
	Code          string
}

func multipleFile(w http.ResponseWriter, r *http.Request) {
	//定义value 为string 类型的字典，用来存合约hash,合约编译器，文件名字
	var m1 = make(map[string]string)
	//定义value 为int 类型的字典，用来存合约更新次数，合约id
	var m2 = make(map[string]int)
	//声明一个http数据接收器
	reader, err := r.MultipartReader()
	//根据当前时间戳来创建文件夹，用来存放合约作者要上传的合约源文件
	pathFile, folderName := createDateDir("./")
	if err != nil {
		fmt.Println("stop here")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 读取作者上传的文件以及ContractHash,CompilerVersion等数据，并保存在map中。
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		//fmt.Printf("FileName =[%S], FormName=[%s]\n", part.FileName(), part.FormName())

		if part.FileName() == "" {
			data, _ := ioutil.ReadAll(part)
			//fmt.Printf("FormName=[%s] FormData=[%s]\n",part.FormName(), string(data))
			//fmt.Println(part.FormName())
			if part.FormName() == "Contract" {
				m1[part.FormName()] = string(data)
				//fmt.Println(m1)
			} else if part.FormName() == "Version" {
				m1[part.FormName()] = string(data)
				//fmt.Println(m1)
			} else if part.FormName() == "CompileCommand" {
				m1[part.FormName()] = string(data)
			} else if part.FormName() == "JavaPackage" {
				m1[part.FormName()] = string(data)
			}
		} else {
			//dst,_ :=os.Create("./"+part.FileName()
			dst, _ := os.OpenFile(pathFile+"/"+part.FileName(), os.O_WRONLY|os.O_CREATE, 0666)
			defer dst.Close()
			io.Copy(dst, part)
			fileExt := path.Ext(pathFile + "/" + part.FileName())
			if fileExt == ".csproj" {
				point := strings.Index(part.FileName(), ".")
				tmp := part.FileName()[0:point]
				m1["Filename"] = tmp
			} else if fileExt == ".py" {
				point := strings.Index(part.FileName(), ".")
				tmp := part.FileName()[0:point]
				m1["Filename"] = tmp
			} else if fileExt == ".java" {
				point := strings.Index(part.FileName(), ".")
				tmp := part.FileName()[0:point]
				m1["Filename"] = tmp
			}

		}

	}

	//编译用户上传的合约源文件，并返回编译后的.nef数据
	chainNef := execCommand(pathFile, folderName, w, m1)
	//如果编译出错，程序不向下执行
	if chainNef == "0" || chainNef == "1" || chainNef == "2" {
		return

	}
	//向链上结点请求合约的状态，返回请求到的合约nef数据
	version, sourceNef := getContractState(pathFile, w, m1, m2)
	//如果请求失败，程序不向下执行
	if sourceNef == "3" || sourceNef == "4" {
		return
	}
	//比较用户上传的源代码编译的.nef文件与链上存储的合约.nef数据是否相等，如果相等的话，向数据库插入数据
	if sourceNef == chainNef {
		//打开数据库配置文件
		cfg, err := OpenConfigFile()
		if err != nil {
			log.Fatal(" open file error")
		}
		//连接数据库
		ctx := context.TODO()
		co, dbonline := intializeMongoOnlineClient(cfg, ctx)
		rt := os.ExpandEnv("${RUNTIME}")
		//查询当前合约是否已经存在于VerifiedContract表中，参数为合约hash，合约更新次数
		filter := bson.M{"hash": getContract(m1), "updatecounter": getUpdateCounter(m2)}
		var result *mongo.SingleResult
		result = co.Database(dbonline).Collection("VerifyContractModel").FindOne(ctx, filter)

		//如果合约不存在于VerifiedContract表中，验证成功
		if result.Err() != nil {
			//在VerifyContract表中插入该合约信息
			verified := insertVerifiedContract{getContract(m1), getId(m2), getUpdateCounter(m2)}
			var insertOne *mongo.InsertOneResult
			insertOne, err = co.Database(dbonline).Collection("VerifyContractModel").InsertOne(ctx, verified)
			fmt.Println("Connect to mainnet database")
			
			if err != nil {
				log.Fatal(err)
			}
			fmt.Println("Inserted a verified Contract in verifyContractModel collection in"+rt+" database", insertOne.InsertedID)
			//在ContractSourceCode表中，插入上传的合约源代码。
			rd, err := ioutil.ReadDir(pathFile + "/")
			if err != nil {
				fmt.Println(err)
			}
			for _, fi := range rd {
				if fi.IsDir() {
					continue
				} else {
					if getVersion(m1) == "neo3-boa" {
						fileExt := path.Ext(fi.Name())
						if fileExt != ".py" {
							continue
						}
					} else if getVersion(m1) == "neow3j" {
						fileExt := path.Ext(fi.Name())
						if fileExt != ".java" {
							continue
						}
					} else if getVersion(m1) == "neo-go" {
						fileExt := path.Ext(fi.Name())
						if fileExt != ".go" {
							continue
						}
					}
					fmt.Println(fi.Name())
					file, err := os.Open(pathFile + "/" + fi.Name())
					if err != nil {
						log.Fatal(err)
					}
					defer file.Close()
					fileinfo, err := file.Stat()
					if err != nil {
						log.Fatal(err)
					}
					filesize := fileinfo.Size()
					buffer := make([]byte, filesize)
					_, err = file.Read(buffer)
					if err != nil {
						log.Fatal(err)

					}

					var insertOneSourceCode *mongo.InsertOneResult
					sourceCode := insertContractSourceCode{getContract(m1), getUpdateCounter(m2), fi.Name(), string(buffer)}
					if rt == "mainnet" {
						insertOneSourceCode, err = co.Database(dbonline).Collection("ContractSourceCode").InsertOne(ctx, sourceCode)
					} else {
						insertOneSourceCode, err = co.Database(dbonline).Collection("ContractSourceCode").InsertOne(ctx, sourceCode)
					}

					if err != nil {
						log.Fatal(err)
					}
					fmt.Println("Inserted a contract source code in contractSourceCode collection in "+rt+"database", insertOneSourceCode.InsertedID)

				}
			}
			fmt.Println("=================Insert verified contract in database===============")
			msg, _ := json.Marshal(jsonResult{5, "Verify done and record verified contract in database!"})
			w.Header().Set("Content-Type", "application/json")
			os.Rename(pathFile, getContract(m1))
			w.Write(msg)
			//如果合约存在于VerifiedContract表中，说明合约已经被验证过，不会存新的数据
		} else {
			fmt.Println("=================This contract has already been verified===============")
			msg, _ := json.Marshal(jsonResult{6, "This contract has already been verified"})
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Content-Type", "application/json")
			os.RemoveAll(pathFile)
			w.Write(msg)

		}

		////比较用户上传的源代码编译的.nef文件与链上存储的合约.nef数据是否相等，如果不等的话，返回以下内容
	} else {
		fmt.Println(version)
		fmt.Println("=================Your source code doesn't match the contract on bloackchain===============")
		msg, _ := json.Marshal(jsonResult{8, "Contract Source Code Verification error!"})
		w.Header().Set("Content-Type", "application/json")
		w.Write(msg)

	}

}

// 根据上传文件的时间戳来命名新生成的文件夹
func createDateDir(basepath string) (string, string) {
	folderName := time.Now().Format("20060102150405")
	fmt.Println("Create folder " + folderName)
	folderPath := filepath.Join(basepath, folderName)
	if _, err := os.Stat(folderPath); os.IsNotExist(err) {
		os.Mkdir(folderPath, 0777)
		os.Chmod(folderPath, 0777)
	}
	return folderPath, folderName

}

//编译用户上传的合约源码
func execCommand(pathFile string, folderName string, w http.ResponseWriter, m map[string]string) string {
	//cmd := exec.Command("ls")
	//根据用户上传参数选择对应的编译器
	cmd := exec.Command("echo")
	if getVersion(m) == "neo3-boa 0.11.4" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa114")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.11.4")
	}else if getVersion(m) == "neo3-boa 0.11.3" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa113")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.11.3")
	}else if getVersion(m) == "neo3-boa 0.11.2" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa112")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.11.2")
	}else if getVersion(m) == "neo3-boa 0.11.1" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa111")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.11.1")
	}else if getVersion(m) == "neo3-boa 0.11.0" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa110")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.11.0")
	}else if getVersion(m) == "neo3-boa 0.10.1" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa101")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.10.1")
	}else if getVersion(m) == "neo3-boa 0.10.0" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa100")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.10.0")
	}else if getVersion(m) == "neo3-boa 0.9.0" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa090")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.9.0")
	}else if getVersion(m) == "neo3-boa 0.8.3" {
		cmd = exec.Command("/bin/sh",  "/go/application/pythonExec.sh","boa083")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.8.3")
	}else if getVersion(m) == "neo3-boa 0.8.2" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa082")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.8.2")
	}else if getVersion(m) == "neo3-boa 0.8.1" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa081")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.8.1")
	}else if getVersion(m) == "neo3-boa 0.8.0" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa080")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.8.0")
	}else if getVersion(m) == "neo3-boa 0.7.0" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa070")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.7.0")
	}else if getVersion(m) == "neo3-boa 0.3.0" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa030")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.3.0")
	}else if getVersion(m) == "neo3-boa 0.0.3" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa003")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.0.3")
	}else if getVersion(m) == "neo3-boa 0.0.0" {
		cmd = exec.Command("/bin/sh","/go/application/pythonExec.sh","boa000")
		fmt.Println("Compiler: neo3-boa, Command: neo3-boa 0.0.0")
	}else if getVersion(m) == "neow3j" {
		command := "/go/application/javaExec.sh " + getJavaPackage(m) + " " + folderName
		cmd = exec.Command("/bin/sh", "-c", command)
		fmt.Println(command, "Compiler: neow3j, Command:"+"/go/application/javaExec.sh "+getJavaPackage(m)+" "+folderName)
	} else if getVersion(m) == "neo-go" {
		cmd = exec.Command("/bin/sh", "-c", "/go/application/goExec.sh")
		fmt.Println("Compiler: neo-go, Command: neo-go")
	} else if getVersion(m) == "Neo.Compiler.CSharp 3.0.0" {
		if getCompileCommand(m) == "nccs --no-optimize" {
			cmd = exec.Command("/go/application/c/nccs", "--no-optimize")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.0.0, Command: nccs --no-optimize")
		}
		if getCompileCommand(m) == "nccs" {
			cmd = exec.Command("/go/application/c/nccs")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.0.0, Command: nccs")
		}

	} else if getVersion(m) == "Neo.Compiler.CSharp 3.0.2" {
		if getCompileCommand(m) == "nccs --no-optimize" {
			cmd = exec.Command("/go/application/b/nccs", "--no-optimize")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.0.2, Command: nccs --no-optimize")
		}
		if getCompileCommand(m) == "nccs" {
			cmd = exec.Command("/go/application/b/nccs")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.0.2, Command: nccs")
		}

	} else if getVersion(m) == "Neo.Compiler.CSharp 3.0.3" {
		if getCompileCommand(m) == "nccs --no-optimize" {
			cmd = exec.Command("/go/application/a/nccs", "--no-optimize")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.0.3, Command: nccs --no-optimize")
		}
		if getCompileCommand(m) == "nccs" {
			cmd = exec.Command("/go/application/a/nccs")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.0.3, Command: nccs")
		}
	} else if getVersion(m) == "Neo.Compiler.CSharp 3.1.0" {
		if getCompileCommand(m) == "nccs --no-optimize" {
			cmd = exec.Command("dotnet", "/go/application/compiler2/3.1/net6.0/nccs.dll", "--no-optimize")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.1.0, Command: nccs --no-optimize")
		}
		if getCompileCommand(m) == "nccs" {
			cmd = exec.Command("dotnet", "/go/application/compiler2/3.1/net6.0/nccs.dll")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.1.0, Command: nccs")
		}
	} else if getVersion(m) == "Neo.Compiler.CSharp 3.3.0" {
		if getCompileCommand(m) == "nccs --no-optimize" {
			cmd = exec.Command("dotnet", "/go/application/compiler2/3.3/net6.0/nccs.dll", "--no-optimize")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.3.0, Command: nccs --no-optimize")
		}
		if getCompileCommand(m) == "nccs" {
			cmd = exec.Command("dotnet", "/go/application/compiler2/3.3/net6.0/nccs.dll")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.3.0, Command: nccs")
		}
	} else if getVersion(m) == "Neo.Compiler.CSharp 3.4.0" {
		if getCompileCommand(m) == "nccs --no-optimize" {
			cmd = exec.Command("dotnet", "/go/application/compiler2/3.4/net6.0/nccs.dll", "--no-optimize")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.4.0, Command: nccs --no-optimize")
		}
		if getCompileCommand(m) == "nccs" {
			cmd = exec.Command("dotnet", "/go/application/compiler2/3.4/net6.0/nccs.dll")
			fmt.Println("Compiler: Neo.Compiler.CSharp 3.4.0, Command: nccs")
		}
	}else {
		fmt.Println("===============Compiler version doesn't exist==============")
		msg, _ := json.Marshal(jsonResult{0, "Compiler version doesn't exist, please choose Neo.Compiler.CSharp 3.0.0/Neo.Compiler.CSharp 3.0.2/Neo.Compiler.CSharp 3.0.3 version"})
		w.Header().Set("Content-Type", "application/json")
		w.Write(msg)
		os.RemoveAll(pathFile)
		return "0"
	}

	if getVersion(m) != "neow3j" {
		cmd.Dir = pathFile + "/"
	}else{
		cmd.Dir = "./"
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	defer stdout.Close()

	err = cmd.Start()
	if err != nil {
		fmt.Println("=============== Cmd execution failed==============")
		msg, _ := json.Marshal(jsonResult{1, "Cmd execution failed "})
		w.Header().Set("Content-Type", "application/json")
		w.Write(msg)
		os.RemoveAll(pathFile)
		return "1"

	}

	opBytes, err := ioutil.ReadAll(stdout)
	if err != nil {
		log.Fatal(err)
	} else {
		fmt.Println(string(opBytes))
	}
	//
	version:=getVersion(m)
	version =strings.Trim(version," ")
	str:=strings.Split(version," ")
	fmt.Println(str[0],version)
	if str[0] == "neo3-boa" {
		file,_:= GetNameBySuffix(pathFile + "/" ,".nef")
		_, err = os.Lstat(pathFile + "/" + file + ".nef")
		fmt.Println("check python nef")
	} else if getVersion(m) == "neo-go" {
		_, err = os.Lstat(pathFile + "/" + "out.nef")
		fmt.Println("check go nef")
	} else if getVersion(m) == "neow3j" {
		files, _ := ioutil.ReadDir("./javacontractgradle/build/neow3j/")
		for _, f := range files {
			if path.Ext("./"+f.Name()) == ".nef" {
				m["Filename"] = f.Name()
				break
			}    
		}
		_, err = os.Lstat("./javacontractgradle/build/neow3j/" + m["Filename"])
		fmt.Println("find java nef file")
	} else {
		//获取当前nef 文件的名称         合约displayname
		file,_:= GetNameBySuffix(pathFile + "/" + "bin/sc/",".nef")
		//_, err = os.Lstat(pathFile + "/" + "bin/sc/" + m["Filename"] + ".nef")

		_, err = os.Lstat(pathFile + "/" + "bin/sc/" + file + ".nef")

		fmt.Println("there")
	}
	fmt.Println(err)
	if !os.IsNotExist(err) {
		var res nef.File

		if str[0] == "neo3-boa" {
			file,_:= GetNameBySuffix(pathFile + "/" ,".nef")
			f, err := ioutil.ReadFile(pathFile + "/" + file + ".nef")
			if err != nil {
				log.Fatal(err)
			}
			res, err = nef.FileFromBytes(f)
			if err != nil {
				log.Fatal("error")
			}
		} else if getVersion(m) == "neow3j"{
			f, err:= ioutil.ReadFile("./javacontractgradle/build/neow3j/"+m["Filename"])
			if err != nil {
				log.Fatal("error")
			}
			res, err = nef.FileFromBytes(f)
			if err != nil {
				log.Fatal("error")
			}
		} else {
			file,_:= GetNameBySuffix(pathFile + "/" + "bin/sc/",".nef")

			f, err := ioutil.ReadFile(pathFile + "/" + "bin/sc/" + file + ".nef")
			if err != nil {
				log.Fatal(err)
			}
			res, err = nef.FileFromBytes(f)
			if err != nil {
				log.Fatal("error")
			}
		}

		//fmt.Println(res.Script)
		var result = base64.StdEncoding.EncodeToString(res.Script)

		fmt.Println("===========Now is soucre code============")
		fmt.Println(result)
		return result

	} else {

		fmt.Println("============.nef file doesn't exist===========", err)
		msg, _ := json.Marshal(jsonResult{2, ".nef file doesn't exist "})
		w.Header().Set("Content-Type", "application/json")
		w.Write(msg)

		return "2"

	}

}
func verifyNef(name string) string {
	f, err := ioutil.ReadFile("./" + name + ".nef")
	if err != nil {
		log.Fatal(err)
	}
	res, err := nef.FileFromBytes(f)
	if err != nil {
		log.Fatal("error")
	}
	//fmt.Println(res.Script)
	var result = base64.StdEncoding.EncodeToString(res.Script)

	fmt.Println("========== " + name + " ============")
	fmt.Println(result)
	return result

}

// 向链上结点请求合约的nef数据
func getContractState(pathFile string, w http.ResponseWriter, m1 map[string]string, m2 map[string]int) (string, string) {
	rt := os.ExpandEnv("${RUNTIME}")
	var resp *http.Response
	payload, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "getcontractstate",
		"params": []interface{}{
			getContract(m1),
		},
		"id": 1,
	})
	if rt != "mainnet" && rt != "testnet" && rt != "testmagnet" {
		rt = "mainnet"
	}
	fmt.Println("RPC params: ContractHash:" + getContract(m1))
	switch rt {
	case "mainnet":
		resp, err = http.Post(RPCNODEMAIN, "application/json", bytes.NewReader(payload))
		fmt.Println("Runtime is:" + rt)
	case "testnet":
		resp, err = http.Post(RPCNODETEST, "application/json", bytes.NewReader(payload))
		fmt.Println("Runtime is:" + rt)
	case "testmagnet":
		resp, err = http.Post(RPCNODETESTMAGNET, "application/json", bytes.NewReader(payload))
		fmt.Println("Runtime is:" + rt)
	}

	if err != nil {
		fmt.Println("=================RPC Node doesn't exsite===============")
		msg, _ := json.Marshal(jsonResult{3, "RPC Node doesn't exsite! "})
		w.Header().Set("Content-Type", "application/json")
		w.Write(msg)
		os.RemoveAll(pathFile)
		return "", "3"
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)

	if gjson.Get(string(body), "error").Exists() {
		message := gjson.Get(string(body), "error.message").String()
		fmt.Println("=================" + message + "===============")
		msg, _ := json.Marshal(jsonResult{4, message})
		w.Header().Set("Content-Type", "application/json")
		w.Write(msg)
		os.RemoveAll(pathFile)
		return "", "4"
	}

	nef := gjson.Get(string(body), "result.nef.script")
	version := gjson.Get(string(body), "result.nef.compiler").String()
	updateCounter := gjson.Get(string(body), "result.updatecounter").String()
	id := gjson.Get(string(body), "result.id").String()
	m2["id"], _ = strconv.Atoi(id)
	m2["updateCounter"], _ = strconv.Atoi(updateCounter)
	//fmt.Println(base64.StdEncoding.DecodeString(sourceNef))
	fmt.Println("===============Now is ChainNode nef===============")
	fmt.Println(nef.String())
	return version, nef.String()

}

func OpenConfigFile() (Config, error) {
	absPath, _ := filepath.Abs("config.yml")
	f, err := os.Open(absPath)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	var cfg Config
	decoder := yaml.NewDecoder(f)
	err = decoder.Decode(&cfg)
	if err != nil {
		return Config{}, err
	}
	return cfg, err
}

//链接主网和测试网数据库
func intializeMongoOnlineClient(cfg Config, ctx context.Context) (*mongo.Client, string) {
	rt := os.ExpandEnv("${RUNTIME}")
	var clientOptions *options.ClientOptions
	var dbOnline string
	if rt != "mainnet" && rt != "testnet" && rt != "testmagnet"{
		rt = "mainnet"
	}
	switch rt {
	case "mainnet":
		clientOptions = options.Client().ApplyURI("mongodb://" + cfg.Database_main.User + ":" + cfg.Database_main.Pass + "@" + cfg.Database_main.Host + ":" + cfg.Database_main.Port + "/" + cfg.Database_main.Database)
		dbOnline = cfg.Database_main.Database
	case "testnet":
		clientOptions = options.Client().ApplyURI("mongodb://" + cfg.Database_test.User + ":" + cfg.Database_test.Pass + "@" + cfg.Database_test.Host + ":" + cfg.Database_test.Port + "/" + cfg.Database_test.Database)
		dbOnline = cfg.Database_test.Database
	case "testmagnet":
		clientOptions = options.Client().ApplyURI("mongodb://" + cfg.Database_testmagnet.User + ":" + cfg.Database_testmagnet.Pass + "@" + cfg.Database_testmagnet.Host + ":" + cfg.Database_testmagnet.Port + "/" + cfg.Database_testmagnet.Database)
		dbOnline = cfg.Database_testmagnet.Database
	}

	clientOptions.SetMaxPoolSize(50)
	co, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		log.Fatal("momgo connect error")
	}
	err = co.Ping(ctx, nil)
	if err != nil {
		log.Fatal("ping mongo error")
	}
	fmt.Println("Connect mongodb success")

	fmt.Println(dbOnline)

	return co, dbOnline
}

//获取目录下以××后缀的文件名（单个文件）
func GetNameBySuffix(path string,suffix string) (string ,bool){
	fileList,_:=ioutil.ReadDir(path)

	for _,it:=range fileList {
		name := it.Name()
		compileRegex := regexp.MustCompile(suffix+"$")
		isExit := compileRegex.MatchString(name)
		if isExit {
			file := strings.TrimSuffix(name, suffix) // 输出 name
			return file,true
		}
	}

	return "", false
}
func getContract(m map[string]string) string {
	return m["Contract"]
}

func getVersion(m map[string]string) string {
	return m["Version"]
}

func getUpdateCounter(m map[string]int) int {
	return m["updateCounter"]
}

func getId(m map[string]int) int {
	return m["id"]
}
func getCompileCommand(m map[string]string) string {
	return m["CompileCommand"]
}
func getJavaPackage(m map[string]string) string {
	return m["JavaPackage"]
}

//监听127.0.0.1:1926端口
func main() {

	fmt.Println("Server start")
	fmt.Println("YOUR ENV IS " + os.ExpandEnv("${RUNTIME}"))
	//verifyNef("helloword")
	//verifyNef("FTWContract_twotag")
	//verifyNef("FTWContract_nooptimizetag")
	//verifyNef("FTWContract_debugtag")
	//fmt.Println("VwABDANGVFdAVwABeDUGAAAAQFcAAXg1BgAAAEBXAAF4NQYAAABAVwABQFcAARhADAEA2zBBm/ZnzkGSXegxStgmBEUQ2yFAStgmBEUQ2yFAQZJd6DFAQZv2Z85AVwIBIXhwaAuXJw0AAAAR2yAjEQAAACF4StkoUMoAFLOrqiEnKAAAAAwgVGhlIGFyZ3VtZW50ICJvd25lciIgaXMgaW52YWxpZC46IUGb9mfOERGIThBR0FASwHFpeEsRzlCLUBDOQZJd6DFK2CYERRDbISMFAAAAQErZKFDKABSzq0ARiE4QUdBQEsBASxHOUItQEM5Bkl3oMUBXAwEhQZv2Z85wDAEA2zBxaWhBkl3oMUrYJgRFENshcmp4nkpyRWppaEHmPxiEQEHmPxiEQFcCAiFBm/ZnzhERiE4QUdBQEsBwaHhLEc5Qi1AQzkGSXegxStgmBEUQ2yFxaXmeSnFFaRC1Jw0AAAAQ2yAjPQAAACFpELMnGQAAAGh4SxHOUItQEM5BL1jF7SMXAAAAIWh4aRJNEc5Ri1EQzkHmPxiEIRHbICMFAAAAQEsRzlCLUBDOQS9Yxe1AEk0RzlGLURDOQeY/GIRAVwIEIXhwaAuXJw0AAAAR2yAjEQAAACF4StkoUMoAFLOrqiEnJwAAAAwfVGhlIGFyZ3VtZW50ICJmcm9tIiBpcyBpbnZhbGlkLjoheXFpC5cnDQAAABHbICMRAAAAIXlK2ShQygAUs6uqISclAAAADB1UaGUgYXJndW1lbnQgInRvIiBpcyBpbnZhbGlkLjohehC1Jy0AAAAMJVRoZSBhbW91bnQgbXVzdCBiZSBhIHBvc2l0aXZlIG51bWJlci46IXhB+CfsjKonDQAAABDbICNBAAAAIXoQmCcmAAAAIXqbeDWG/v//qicNAAAAENsgIyEAAAAhenk1cP7//0UhIXt6eXg1FAAAABHbICMFAAAAQEH4J+yMQFcCBCHCSnjPSnnPSnrPDAhUcmFuc2ZlckGVAW9heXBoC5eqJQ0AAAAQ2yAjDwAAACF5NwAAcWkLl6ohJyIAAAB7engTwB8MDm9uTkVQMTdQYXltZW50eUFifVtSRSFANwAAQEFifVtSQFcAAiF5mRC1Jw4AAAAMBmFtb3VudDoheRCzJwoAAAAjHQAAACF5eDXA/f//RXk1hP3//wt5eAs1YP///0BXAAIheZkQtScOAAAADAZhbW91bnQ6IXkQsycKAAAAIzEAAAAheZt4NYL9//+qJxEAAAAMCWV4Y2VwdGlvbjoheZs1M/3//wt5C3g1D////0BXAQIheHBoC5cnDQAAABHbICMRAAAAIXhK2ShQygAUs6uqIScnAAAADB9UaGUgYXJndW1lbnQgImZyb20iIGlzIGludmFsaWQuOiF4Qfgn7IyqJxkAAAAMEU5vIGF1dGhvcml6YXRpb24uOiF5eDVB////QFcDAiF5JwoAAAAjXAAAACE12Pv//xC3JyEAAAAMGUNvbnRyYWN0IGFscmVheSBkZXBsb3llZC46IUEtUQgwcAwB/9swcWgTzmlBm/ZnzkHmPxiEAwAAxS68orEAcmpoE841nf7//0BBLVEIMEBB5j8YhEBXAwIhDAH/2zBwaEGb9mfOQZJd6DFK2CUPAAAASsoAFCkGAAAAOiFxQS1RCDByaWoTzpclDQAAABDbICMMAAAAIWlB+CfsjCEnEgAAACELeXg3AQAhIzYAAAAhIQwrT25seSBjb250cmFjdCBvd25lciBjYW4gdXBkYXRlIHRoZSBjb250cmFjdDohIUA3AQBAVwADIQwkUGF5bWVudCBpcyBkaXNhYmxlIG9uIHRoaXMgY29udHJhY3QhOkBWAQqx+v//CoH6//8SwGBAwkpYz0o1fPr//yNu+v//wkpYz0o1bfr//yOK+v//"=="VwABDANGVFdAVwABeDUGAAAAQFcAAXg1BgAAAEBXAAF4NQYAAABAVwABQFcAARhADAEA2zBBm/ZnzkGSXegxStgmBEUQ2yFAStgmBEUQ2yFAQZJd6DFAQZv2Z85AVwIBIXhwaAuXJw0AAAAR2yAjEQAAACF4StkoUMoAFLOrqiEnKAAAAAwgVGhlIGFyZ3VtZW50ICJvd25lciIgaXMgaW52YWxpZC46IUGb9mfOERGIThBR0FASwHFpeEsRzlCLUBDOQZJd6DFK2CYERRDbISMFAAAAQErZKFDKABSzq0ARiE4QUdBQEsBASxHOUItQEM5Bkl3oMUBXAwEhQZv2Z85wDAEA2zBxaWhBkl3oMUrYJgRFENshcmp4nkpyRWppaEHmPxiEQEHmPxiEQFcCAiFBm/ZnzhERiE4QUdBQEsBwaHhLEc5Qi1AQzkGSXegxStgmBEUQ2yFxaXmeSnFFaRC1Jw0AAAAQ2yAjPQAAACFpELMnGQAAAGh4SxHOUItQEM5BL1jF7SMXAAAAIWh4aRJNEc5Ri1EQzkHmPxiEIRHbICMFAAAAQEsRzlCLUBDOQS9Yxe1AEk0RzlGLURDOQeY/GIRAVwIEIXhwaAuXJw0AAAAR2yAjEQAAACF4StkoUMoAFLOrqiEnJwAAAAwfVGhlIGFyZ3VtZW50ICJmcm9tIiBpcyBpbnZhbGlkLjoheXFpC5cnDQAAABHbICMRAAAAIXlK2ShQygAUs6uqISclAAAADB1UaGUgYXJndW1lbnQgInRvIiBpcyBpbnZhbGlkLjohehC1Jy0AAAAMJVRoZSBhbW91bnQgbXVzdCBiZSBhIHBvc2l0aXZlIG51bWJlci46IXhB+CfsjKonDQAAABDbICNBAAAAIXoQmCcmAAAAIXqbeDWG/v//qicNAAAAENsgIyEAAAAhenk1cP7//0UhIXt6eXg1FAAAABHbICMFAAAAQEH4J+yMQFcCBCHCSnjPSnnPSnrPDAhUcmFuc2ZlckGVAW9heXBoC5eqJQ0AAAAQ2yAjDwAAACF5NwAAcWkLl6ohJyIAAAB7engTwB8MDm9uTkVQMTdQYXltZW50eUFifVtSRSFANwAAQEFifVtSQFcAAiF5mRC1Jw4AAAAMBmFtb3VudDoheRCzJwoAAAAjHQAAACF5eDXA/f//RXk1hP3//wt5eAs1YP///0BXAAIheZkQtScOAAAADAZhbW91bnQ6IXkQsycKAAAAIzEAAAAheZt4NYL9//+qJxEAAAAMCWV4Y2VwdGlvbjoheZs1M/3//wt5C3g1D////0BXAQIheHBoC5cnDQAAABHbICMRAAAAIXhK2ShQygAUs6uqIScnAAAADB9UaGUgYXJndW1lbnQgImZyb20iIGlzIGludmFsaWQuOiF4Qfgn7IyqJxkAAAAMEU5vIGF1dGhvcml6YXRpb24uOiF5eDVB////QFcDAiF5JwoAAAAjXAAAACE12Pv//xC3JyEAAAAMGUNvbnRyYWN0IGFscmVheSBkZXBsb3llZC46IUEtUQgwcAwB/9swcWgTzmlBm/ZnzkHmPxiEAwAAxS68orEAcmpoE841nf7//0BBLVEIMEBB5j8YhEBXAwIhDAH/2zBwaEGb9mfOQZJd6DFK2CUPAAAASsoAFCkGAAAAOiFxQS1RCDByaWoTzpclDQAAABDbICMMAAAAIWlB+CfsjCEnEgAAACELeXg3AQAhIzYAAAAhIQwrT25seSBjb250cmFjdCBvd25lciBjYW4gdXBkYXRlIHRoZSBjb250cmFjdDohIUA3AQBAVwADIQwkUGF5bWVudCBpcyBkaXNhYmxlIG9uIHRoaXMgY29udHJhY3QhOkBWAQqx+v//CoH6//8SwGBAwkpYz0o1fPr//yNu+v//wkpYz0o1bfr//yOK+v//")
	//fmt.Println("VwABDANGVFdAVwABeDQDQFcAAXg0A0BXAAF4NANAVwABQFcAARhADAEA2zBBm/ZnzkGSXegxStgmBEUQ2yFAStgmBEUQ2yFAQZJd6DFAQZv2Z85AVwEBeHBoC5cmBxHbICINeErZKFDKABSzq6omJQwgVGhlIGFyZ3VtZW50ICJvd25lciIgaXMgaW52YWxpZC46QZv2Z84REYhOEFHQUBLAcGh4SxHOUItQEM5Bkl3oMUrYJgRFENshIgJAStkoUMoAFLOrQBGIThBR0FASwEBLEc5Qi1AQzkGSXegxQFcDAUGb9mfOcAwBANswcWloQZJd6DFK2CYERRDbIXJqeJ5KckVqaWhB5j8YhEBB5j8YhEBXAgJBm/ZnzhERiE4QUdBQEsBwaHhLEc5Qi1AQzkGSXegxStgmBEUQ2yFxaXmeSnFFaRC1JgcQ2yAiLmkQsyYTaHhLEc5Qi1AQzkEvWMXtIhNoeGkSTRHOUYtREM5B5j8YhBHbICICQEsRzlCLUBDOQS9Yxe1AEk0RzlGLURDOQeY/GIRAVwEEeHBoC5cmBxHbICINeErZKFDKABSzq6omJAwfVGhlIGFyZ3VtZW50ICJmcm9tIiBpcyBpbnZhbGlkLjp5cGgLlyYHEdsgIg15StkoUMoAFLOrqiYiDB1UaGUgYXJndW1lbnQgInRvIiBpcyBpbnZhbGlkLjp6ELUmKgwlVGhlIGFtb3VudCBtdXN0IGJlIGEgcG9zaXRpdmUgbnVtYmVyLjp4Qfgn7IyqJgcQ2yAiKnoQmCYaept4NcH+//+qJgcQ2yAiFXp5NbL+//9Fe3p5eDQOEdsgIgJAQfgn7IxAVwEEwkp4z0p5z0p6zwwIVHJhbnNmZXJBlQFvYXlwaAuXqiQHENsgIgt5NwAAcGgLl6omH3t6eBPAHwwOb25ORVAxN1BheW1lbnR5QWJ9W1JFQDcAAEBBYn1bUkBXAAJ5mRC1JgsMBmFtb3VudDp5ELMmBCIZeXg1I/7//0V5Nej9//8LeXgLNXn///9AVwACeZkQtSYLDAZhbW91bnQ6eRCzJgQiKXmbeDXx/f//qiYODAlleGNlcHRpb246eZs1p/3//wt5C3g1OP///0BXAQJ4cGgLlyYHEdsgIg14StkoUMoAFLOrqiYkDB9UaGUgYXJndW1lbnQgImZyb20iIGlzIGludmFsaWQuOnhB+CfsjKomFgwRTm8gYXV0aG9yaXphdGlvbi46eXg1Yv///0BXAwJ5JgQiVDV1/P//ELcmHgwZQ29udHJhY3QgYWxyZWF5IGRlcGxveWVkLjpBLVEIMHAMAf/bMHFoE85pQZv2Z85B5j8YhAMAAMUuvKKxAHJqaBPONdb+//9AQS1RCDBAQeY/GIRAVwMCDAH/2zBwaEGb9mfOQZJd6DFK2CQJSsoAFCgDOnFBLVEIMHJpahPOlyQHENsgIghpQfgn7IwmCgt5eDcBACIwDCtPbmx5IGNvbnRyYWN0IG93bmVyIGNhbiB1cGRhdGUgdGhlIGNvbnRyYWN0OkA3AQBAVwADDCRQYXltZW50IGlzIGRpc2FibGUgb24gdGhpcyBjb250cmFjdCE6QFYBCm/7//8KSPv//xLAYEDCSljPSjVD+///IzX7///CSljPSjU0+///I0j7//8="=="VwABDANGVFdAVwABeDQDQFcAAXg0A0BXAAF4NANAVwABQFcAARhADAEA2zBBm/ZnzkGSXegxStgmBEUQ2yFAStgmBEUQ2yFAQZJd6DFAQZv2Z85AVwEBeHBoC5cmBxHbICINeErZKFDKABSzq6omJQwgVGhlIGFyZ3VtZW50ICJvd25lciIgaXMgaW52YWxpZC46QZv2Z84REYhOEFHQUBLAcGh4SxHOUItQEM5Bkl3oMUrYJgRFENshIgJAStkoUMoAFLOrQBGIThBR0FASwEBLEc5Qi1AQzkGSXegxQFcDAUGb9mfOcAwBANswcWloQZJd6DFK2CYERRDbIXJqeJ5KckVqaWhB5j8YhEBB5j8YhEBXAgJBm/ZnzhERiE4QUdBQEsBwaHhLEc5Qi1AQzkGSXegxStgmBEUQ2yFxaXmeSnFFaRC1JgcQ2yAiLmkQsyYTaHhLEc5Qi1AQzkEvWMXtIhNoeGkSTRHOUYtREM5B5j8YhBHbICICQEsRzlCLUBDOQS9Yxe1AEk0RzlGLURDOQeY/GIRAVwEEeHBoC5cmBxHbICINeErZKFDKABSzq6omJAwfVGhlIGFyZ3VtZW50ICJmcm9tIiBpcyBpbnZhbGlkLjp5cGgLlyYHEdsgIg15StkoUMoAFLOrqiYiDB1UaGUgYXJndW1lbnQgInRvIiBpcyBpbnZhbGlkLjp6ELUmKgwlVGhlIGFtb3VudCBtdXN0IGJlIGEgcG9zaXRpdmUgbnVtYmVyLjp4Qfgn7IyqJgcQ2yAiKnoQmCYaept4NcH+//+qJgcQ2yAiFXp5NbL+//9Fe3p5eDQOEdsgIgJAQfgn7IxAVwEEwkp4z0p5z0p6zwwIVHJhbnNmZXJBlQFvYXlwaAuXqiQHENsgIgt5NwAAcGgLl6omH3t6eBPAHwwOb25ORVAxN1BheW1lbnR5QWJ9W1JFQDcAAEBBYn1bUkBXAAJ5mRC1JgsMBmFtb3VudDp5ELMmBCIZeXg1I/7//0V5Nej9//8LeXgLNXn///9AVwACeZkQtSYLDAZhbW91bnQ6eRCzJgQiKXmbeDXx/f//qiYODAlleGNlcHRpb246eZs1p/3//wt5C3g1OP///0BXAQJ4cGgLlyYHEdsgIg14StkoUMoAFLOrqiYkDB9UaGUgYXJndW1lbnQgImZyb20iIGlzIGludmFsaWQuOnhB+CfsjKomFgwRTm8gYXV0aG9yaXphdGlvbi46eXg1Yv///0BXAwJ5JgQiVDV1/P//ELcmHgwZQ29udHJhY3QgYWxyZWF5IGRlcGxveWVkLjpBLVEIMHAMAf/bMHFoE85pQZv2Z85B5j8YhAMAAMUuvKKxAHJqaBPONdb+//9AQS1RCDBAQeY/GIRAVwMCDAH/2zBwaEGb9mfOQZJd6DFK2CQJSsoAFCgDOnFBLVEIMHJpahPOlyQHENsgIghpQfgn7IwmCgt5eDcBACIwDCtPbmx5IGNvbnRyYWN0IG93bmVyIGNhbiB1cGRhdGUgdGhlIGNvbnRyYWN0OkA3AQBAVwADDCRQYXltZW50IGlzIGRpc2FibGUgb24gdGhpcyBjb250cmFjdCE6QFYBCm/7//8KSPv//xLAYEDCSljPSjVD+///IzX7///CSljPSjU0+///I0j7//8=")
	mux := http.NewServeMux()
	mux.HandleFunc("/upload", func(writer http.ResponseWriter, request *http.Request) {
		multipleFile(writer, request)
	})
	mux.Handle("/", promhttp.Handler())
	handler := cors.Default().Handler(mux)
	err := http.ListenAndServe("0.0.0.0:1927", handler)
	if err != nil {
		fmt.Println("listen and server error")
	}
}
